import { DELIVERY_CAPABILITIES, type MessageDeliveryMode } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useCallback, useState } from "react";
import { Link } from "react-router-dom";

import { agentMessageRequest, apiErrorMessage } from "../../api/client";
import { agentMessagesQuery, whoAmIQuery } from "../../api/queries";
import type { Message } from "../../api/types";
import { ContextRing } from "../../components/ContextRing";
import { PinButton } from "../../components/PinButton";
import { AgentListRefilling } from "../../components/QueryError";
import { TruncatedText } from "../../components/TruncatedText";
import {
  connectionDotConnecting,
  connectionDotFailed,
  dangerText,
  inputClasses,
  linkHoverText,
  linkText,
  liveDotBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { useAgentPinWrite, usePinnedAgents } from "../agents/pins";
import { useMarkRepliesRead } from "../agents/unread";
import { deliveryModesOf, MODE_LABELS, noDeliveryLine } from "../conversation/delivery";
import { useAgents } from "../conversation/useAgents";
import { useSessionSends } from "../conversation/useSessionSends";
import { AgentName, AgentUnresponsiveNote, useAgentText } from "../refs/AgentName";
import { ErrorBoundary } from "../shell/ErrorBoundary";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { SendStoppedError } from "../writes/keyed-write";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { AgentRuntimeThread } from "./AgentRuntimeThread";
import { sessionCommands, sessionSettingsView } from "./conversation";
import { MessageBoxHelp } from "./MessageBoxHelp";
import { SessionSettings, SessionSwitchLines } from "./SessionSettings";
import { type SwitchRequest, switchMessageInput, useSessionSwitch } from "./use-session-switch";
import { type AgentStreamStatus, useAgentStream } from "./useAgentStream";

/** What the header's presence can say: the relay connection and whether the session answers it,
 *  or, for a session a current list no longer holds, that fact (`not-listed`, `livePresence`).
 *  Not whether the agent is running, which the header's `AgentName` mark shows. */
type Presence = AgentStreamStatus | "not-listed";

/** Each presence's dot, its words, and how it reads to a screen reader. One with no `title` of
 *  its own names the connection, and is titled and read as one; `not-listed` names a fact of the
 *  list instead. */
const STATUS: Record<Presence, { dot: string; label: string; title?: string }> = {
  connecting: { dot: connectionDotConnecting, label: "Connecting to the session" },
  live: { dot: liveDotBg, label: "Live" },
  "not-answering": { dot: connectionDotFailed, label: "Not answering" },
  "not-listed": {
    dot: connectionDotFailed,
    label: "Not in the session list",
    title: "The live session list no longer holds this session",
  },
  reconnecting: { dot: connectionDotConnecting, label: "Reconnecting" },
  unavailable: { dot: connectionDotFailed, label: "This session's conversation is unavailable" },
};

/** What the registry says of the session, from a current list alone: a list that is refilling,
 *  loading or unreachable says neither. Only a later current list ends either fact: a frame of
 *  the session's does not, so a session that still streams keeps the list's word until the list
 *  says otherwise. */
interface RegistryFacts {
  /** The list holds the session as missing its heartbeats. */
  readonly unresponsive: boolean;
  /** The list no longer holds the session, which this view has seen it hold: it left the
   *  registry. */
  readonly gone: boolean;
}

/** Why the view takes the session to be silent: the relay hears nobody on its control subject
 *  (`responding === false`), or a current list holds the session as unresponsive, or no longer
 *  holds it. */
type Silence = "relay" | "unresponsive" | "gone";

/**
 * Whether the session is silent, and by whose word. The relay and the registry each see a session
 * fall silent in their own time - the relay when nobody answers its control subject, the registry
 * on its poll - so either is enough, and a session that stops while its view is open reads as
 * silent once the first of them notices. The relay's word comes first: it is the one that says
 * nothing streams. This reads the relay's last answer, not the socket, which a reconnect leaves in
 * place until the relay answers again, so a lost socket takes back no text the view showed.
 */
function silenceOf(responding: boolean | undefined, facts: RegistryFacts): Silence | undefined {
  if (responding === false) return "relay";
  if (facts.unresponsive) return "unresponsive";
  if (facts.gone) return "gone";
  return undefined;
}

/**
 * What the header's presence says, or undefined for none. A refusal and a lost socket are the
 * view's own connection, which no silence changes, so they stand as they are, and the transcript
 * goes on saying the silence beneath them. Over a connection that stands, the relay's silence
 * reads Not answering, and a session a current list no longer holds reads as that fact rather
 * than Live. A session the list holds as unresponsive gets no presence: the header's
 * `AgentUnresponsiveNote` names its state and when it was last seen, and is the header's word on
 * it, where Live would deny what the list says and Not answering would deny the turns it may
 * still stream. No frame ends either of the list's facts: only the next current list does.
 */
function livePresence(
  status: AgentStreamStatus,
  silence: Silence | undefined
): Presence | undefined {
  if (status !== "live" && status !== "connecting") return status;
  if (silence === "unresponsive") return undefined;
  if (silence === "gone") return "not-listed";
  return silence === "relay" ? "not-answering" : status;
}

/** The relay's causes for a session nobody answers on its control subject, which both of the
 *  relay's texts below open with. They are the relay's alone: the registry's facts show neither
 *  cause, so its texts name neither. */
const NOT_ANSWERING_WHY =
  "This session is not answering the live view: its plugin does not stream its conversation, or the session is no longer running.";

/** What an empty transcript says while the relay hears nobody from the session (`emptyText`). */
export const NOT_ANSWERING_EMPTY_TEXT = `${NOT_ANSWERING_WHY} Nothing appears here until it answers.`;

/** The line above a transcript that holds turns while the relay hears nobody from the session:
 *  what shows is what Dispatch keeps, and nothing the session streams arrives until it answers. */
export const NOT_ANSWERING_NOTICE = `${NOT_ANSWERING_WHY} Only the messages Dispatch keeps show here until it answers.`;

/** What the transcript says, in its empty slot and above its turns alike, while a current list
 *  holds the session as unresponsive and the relay has not said it is silent: why the list says
 *  so, and that the session has not ended - it keeps its roles and claims
 *  (`AgentUnresponsiveNote`). It says nothing of the stream, which the list does not show: turns
 *  may still arrive under it, and a refused view gets none. No frame ends the list's word, so the
 *  line stays true above turns that arrive under it. */
export const MISSED_HEARTBEATS_TEXT =
  "This session has missed its heartbeats, so the live session list marks it unresponsive. It has not ended.";

/** What the transcript says, in its empty slot and above its turns alike, once a current list no
 *  longer holds a session this view saw it hold and the relay has not said it is silent: what the
 *  list shows, and neither that the session ended nor anything of its stream, which the list does
 *  not say. No frame ends the list's word, so the line stays true above turns that arrive under
 *  it. */
export const LEFT_LIST_TEXT = "The live session list no longer holds this session.";

/** What an empty transcript says of a session that answers the view, or has not said yet. */
export const NOTHING_YET_TEXT = "Nothing yet. This session's next turn appears here as it happens.";

/** What each silence says in the empty slot, and in the line above a transcript that holds turns.
 *  The relay's two differ in what they say shows until the session answers: nothing, or what
 *  Dispatch keeps. A list fact has one text for both, since it promises nothing about the
 *  transcript. */
const SILENCE_TEXT: Record<Silence, { readonly empty: string; readonly notice: string }> = {
  gone: { empty: LEFT_LIST_TEXT, notice: LEFT_LIST_TEXT },
  relay: { empty: NOT_ANSWERING_EMPTY_TEXT, notice: NOT_ANSWERING_NOTICE },
  unresponsive: { empty: MISSED_HEARTBEATS_TEXT, notice: MISSED_HEARTBEATS_TEXT },
};

/** Where each mode stands in the composer. Send comes first wherever the session takes it, since
 *  that is Enter at its terminal, and Aside next, so a session that advertises Aside and no Send
 *  opens on Aside. A mode the contracts add must be placed here before it compiles. */
const MODE_ORDER: Record<MessageDeliveryMode, number> = { aside: 1, btw: 2, steer: 0 };

/** The modes a human can talk to a session in, narrowed to the ones it advertises, in
 *  `MODE_ORDER`; the first is what the composer sends until the human picks. A session that lists
 *  no delivery mode is still offered Aside, so a send to it records a failed attempt, which the
 *  page warns of above the thread. */
function deliveryModes(capabilities: readonly string[]): MessageDeliveryMode[] {
  const advertised = deliveryModesOf(capabilities).sort(
    (left, right) => MODE_ORDER[left] - MODE_ORDER[right]
  );
  return advertised.length === 0 ? ["aside"] : advertised;
}

/**
 * What an empty transcript means for this session: it answers the view, or it is silent
 * (`silenceOf`). Saying "the next turn appears here" to a viewer of a silent session was a
 * promise the page could not keep. A session the relay hears nobody from is not always one with a
 * newer plugin to install - it may have been started without its plugin, or have ended - so the
 * relay's text names what may be true of the session rather than telling its reader to upgrade,
 * and the list's texts say only what the list shows. The relay keeps asking and the registry is
 * read again on its poll, so a session that answers again turns this around without a reload, and
 * the wording stays neutral until either says it is silent. A resumed session answers with the
 * conversation since its process started, so this text is also what a reader sees until such a
 * session takes its next turn. A transcript holding turns says why above them (`SILENCE_TEXT`).
 */
function emptyText(silence: Silence | undefined): string {
  return silence === undefined ? NOTHING_YET_TEXT : SILENCE_TEXT[silence].empty;
}

export function AgentConversationPage({
  front,
  sessionId,
}: {
  /** Whether this is the view the person is looking at. The shell keeps the view they left
   *  mounted for the watch window (`MountedLiveViews`, D6), and a view that is not in front
   *  reads the session as ever while leaving the page in front its title and its read marks
   *  (D7). */
  front: boolean;
  /** The session this view shows. It comes from the shell, not from the route: the view outlives
   *  the address that opened it. */
  sessionId: string;
}): ReactNode {
  const { agents, current, isPending, refilling } = useAgents(true, true);
  const agent = agents.find((candidate) => candidate.session_id === sessionId);
  // The session a current list has held, while this view shows it: only a session the view has
  // seen listed can leave the list. One the list never held may still answer the relay, and its
  // view reads live. Kept by id, so a view moved to another session starts with nothing seen.
  const [seenListed, setSeenListed] = useState<string | null>(null);
  if (current && agent !== undefined && seenListed !== sessionId) setSeenListed(sessionId);
  // The page knows the session id and the registry, not a stamped title: the agent is named by
  // its registry title, else its short id. Only the view in front names the tab.
  const label = useAgentText().sessionText(sessionId, undefined);
  useDocumentTitle(label, front);
  // The pin is the person's, not this browser's (INFRA-812): pinning here lists the session in
  // the rail, as pinning its row on the Agents page does, and either place unpins it.
  const pinned = usePinnedAgents().includes(sessionId);
  const writePin = useAgentPinWrite();

  const { conversation, replays, responding, status } = useAgentStream(sessionId);
  // Whether the session is silent, and by whose word: the relay's, or a current list holding the
  // session as unresponsive or no longer at all. The header's presence reads it over a connection
  // that stands; the empty slot and the line read it whatever the socket does.
  const silence = silenceOf(responding, {
    gone: current && agent === undefined && seenListed === sessionId,
    unresponsive: current && agent?.state === "unresponsive",
  });
  const shown = livePresence(status, silence);
  // The model and thinking level the session runs now (INFRA-195), next to its title: what its
  // newest `settings` frame reports, or, for a session that reports none, the most recent
  // assistant turn's own model (LEGION-548), nothing guessed for a client that never sends one.
  const settings = sessionSettingsView(conversation, responding, replays);
  // Only a current list says what the session takes: while it is refilling or unreachable every
  // mode is offered, as unknown, rather than narrowed to what an empty or earlier list says.
  const modes = deliveryModes(current ? (agent?.capabilities ?? []) : DELIVERY_CAPABILITIES);
  // The human's pick holds only while the session still offers it: the session list loads after
  // the page, and a pick the session does not advertise would be refused.
  const [picked, setPicked] = useState<MessageDeliveryMode | null>(null);
  const mode = picked !== null && modes.includes(picked) ? picked : (modes[0] ?? "aside");
  // Which panel of the footer row stands open, since both open upwards over the transcript from
  // the same corner: one at a time, as a `MultiSelect`'s host holds its list.
  const [panel, setPanel] = useState<"context" | "help" | null>(null);
  // The ring's panel goes with the ring: a session that stops reporting its window takes the ring
  // away, and its next reading draws the ring closed rather than reopening a panel by itself.
  if (panel === "context" && conversation.context === undefined) setPanel(null);
  // The slash commands the composer completes (LEGION-394). The session runs typed input only
  // from a Send or an Aside, so a BTW, a side question it answers, offers none; nor does a
  // session that sent no list, whose host cannot run typed input.
  const listed = sessionCommands(conversation);
  const commands =
    mode === "btw" || listed === undefined || listed.length === 0 ? undefined : listed;

  // The human's direct messages and the session's replies to them, as Dispatch stores them. A
  // person's Send or Aside to a session that takes one becomes the session's own user turn, which
  // the stream carries tagged with this message's id, and the thread shows it once; one the
  // stream carries untagged shows twice (`packages/pi-envoy/AGENTS.md`, the phase-worker section,
  // says when). A BTW, an issue message, and any message to a session that takes no user turn
  // from Envoy arrive as notices the stream has no frame for, and the session answers them with a
  // `dispatch message` command, which the stream shows only as a `bash` tool call whose arguments
  // hold that command. Without these the thread would show replies to messages the human cannot
  // see, and no replies at all.
  const stored = useQuery(agentMessagesQuery(sessionId));
  // Seeing them is reading them, so only the view the person is looking at marks them read, as
  // the Agents page's collapsed row does.
  useMarkRepliesRead(sessionId, front ? stored.data : undefined);
  const viewer = useQuery(whoAmIQuery()).data;
  // The header's switch, one at a time; while one waits for the session's answer neither picker
  // nor Send takes another, so this page's next prompt reaches the session after the switch.
  const sessionSwitch = useSessionSwitch(sessionId, stored.data);
  // One per message of this viewer's the session accepted, which takes the thread back to its
  // live end (`use-live-follow.ts`). Bumped after the write lands, only if the reader is still on
  // the session it went to, and never on a refusal: the reason renders in the message's own turn,
  // where a reader who has scrolled back can read it without being taken away from what they
  // were reading.
  const [ownSendCount, setOwnSendCount] = useState(0);
  // A header pick is its own keyed write, not a composer send: it needs the stored message id to
  // follow the session's answer, and its pending state holds the next message until that answer.
  // It goes in the session's line with every message to it (`session:<id>`), and its refusal is
  // shown here, on the session it went to.
  const queryClient = useQueryClient();
  const switchWrite = useKeyedWrite<Message, { request: SwitchRequest; session: string }>({
    inline: true,
    label: ({ request }) =>
      request.kind === "model"
        ? `Switch ${label}'s model to ${request.value}`
        : `Switch ${label}'s thinking level to ${request.value}`,
    onSuccess: (_message, { session }) => {
      if (session === sessionId) setOwnSendCount((previous) => previous + 1);
      void queryClient.invalidateQueries({ queryKey: agentMessagesQuery(session).queryKey });
    },
    target: ({ session }) => `session:${session}`,
    request: ({ request, session }) => agentMessageRequest(session, switchMessageInput(request)),
  });
  // Each message is a write of its own in the app's write store, held for its session rather than
  // this view, so every attempt carries its key and a refusal stays for Retry or Discard. The
  // session's Agents row shows and acts on the same writes.
  const sends = useSessionSends(sessionId, () => setOwnSendCount((previous) => previous + 1));
  // Each press names its send beneath the live view's own name, not the Agents row's, so a message
  // out from here holds nothing of the row's - its issue picker, its Reply - which addresses its
  // own sends; both surfaces still show every message to the session (`useSessionSends`).
  const onSend = useCallback(
    (body: string) => sends.send(["agent-live", sessionId], body, mode),
    [mode, sends, sessionId]
  );
  // The switch's status follows the whole keyed write, from the press until Dispatch answers,
  // however many attempts that takes.
  const onPick = useCallback(
    (request: SwitchRequest) =>
      void sessionSwitch
        .run(request, () => switchWrite.mutateAsync({ request, session: sessionId }))
        .catch(() => {}),
    [sessionId, sessionSwitch, switchWrite]
  );
  const switchRefusal =
    switchWrite.isError && switchWrite.variables?.session === sessionId ? switchWrite.error : null;
  // A switch holds only the session it went to, from the press until the session answers.
  const switchPending =
    sessionSwitch.pending ||
    (switchWrite.isPending && switchWrite.variables?.session === sessionId);
  const presence = shown === undefined ? undefined : STATUS[shown];
  // The warning of a session whose capabilities hold no delivery mode; undefined for one that
  // takes a mode.
  const noDelivery = noDeliveryLine(agent?.capabilities ?? []);

  // The shell hands this route the viewport below its header as a flex column; the page takes
  // all of it and the thread is its only scroller, so the header and composer never leave the
  // screen and the document never scrolls.
  return (
    <div className="flex min-h-0 flex-1 flex-col" data-testid="agent-conversation">
      <header
        className="flex flex-wrap items-center gap-x-3 gap-y-1"
        data-testid="agent-view-header"
      >
        <Link className={`text-sm ${linkText} ${linkHoverText}`} to="/agents">
          ← Agents
        </Link>
        {/* This is that agent's own live conversation, so its name links nowhere: `AgentName`
            draws it, with its running state, without a link. */}
        <h1 className={`min-w-0 truncate text-lg font-semibold ${textPrimaryOnCanvas}`}>
          <AgentName kind="static" sessionId={sessionId} storedTitle={undefined} />
        </h1>
        {presence === undefined ? null : (
          <span
            className="flex items-center gap-1.5"
            title={presence.title ?? `Connection to the session's conversation: ${presence.label}`}
          >
            <span
              aria-hidden="true"
              className={`inline-block h-2 w-2 rounded-full ${presence.dot}`}
            />
            <span className={`text-xs ${textMutedOnCanvas}`}>
              {presence.title === undefined ? <span className="sr-only">Connection: </span> : null}
              {presence.label}
            </span>
          </span>
        )}
        {current ? null : refilling ? (
          <AgentListRefilling className={`text-xs ${textMutedOnCanvas}`} />
        ) : isPending ? (
          <span className={`text-xs ${textMutedOnCanvas}`} data-testid="agent-list-loading">
            Loading the live session list…
          </span>
        ) : (
          <span className={`text-xs ${dangerText}`} data-testid="agent-list-unreachable">
            Could not check the live session list — Envoy listener unreachable.
          </span>
        )}
        {/* The header holds the session's name and its state (R6); where it runs and what it
            runs on are the row under the message box. A session that has missed its heartbeats
            is listed and named as such, and over a connection that stands this is the header's
            whole word on it (`livePresence`): the difference between waiting for it and giving
            up on it. */}
        {agent === undefined ? null : (
          <AgentUnresponsiveNote
            lastSeen={agent.last_seen}
            state={agent.state === "unresponsive" ? "unresponsive" : "running"}
          />
        )}
        <PinButton
          label={pinned ? `Unpin ${label}` : `Pin ${label}`}
          onClick={() => writePin(sessionId, !pinned)}
          pinned={pinned}
          title={pinned ? "Unpin agent" : "Pin agent"}
        />
      </header>
      {stored.isError ? (
        <p className={`mt-2 text-sm ${dangerText}`}>
          Could not load your messages with this session.
        </p>
      ) : null}
      {status === "unavailable" ? (
        <p className={`mt-2 text-sm ${dangerText}`} data-testid="agent-stream-unavailable">
          This Dispatch cannot reach the session's conversation.
        </p>
      ) : null}
      {/* A line of the page, not of a send, so it stays after a send to the session fails. */}
      {current && agent !== undefined && noDelivery !== undefined ? (
        <p className={`mt-2 text-sm ${dangerText}`} data-testid="agent-takes-no-messages">
          {noDelivery}
        </p>
      ) : null}
      {switchRefusal === null ? null : (
        <p className={`mt-2 text-sm ${dangerText}`} data-testid="agent-send-error">
          {switchRefusal instanceof SendStoppedError
            ? switchRefusal.message
            : `Could not send: ${apiErrorMessage(switchRefusal, "could not reach the session")}`}
        </p>
      )}
      {/* Everything that reads a frame lives inside this boundary, in `AgentRuntimeThread`:
          assistant-ui converts every message while the runtime is built, so a message it
          refuses throws in the render of whichever component calls `useExternalStoreRuntime`.
          While that was this page, this boundary sat above nothing and a bad frame took the
          header and the delivery controls with the thread. The transcript has a boundary of its
          own inside `AgentThread`, which keeps the composer when the thread's own rendering is
          what failed. */}
      <ErrorBoundary region="this conversation" resetKey={sessionId}>
        <AgentRuntimeThread
          commands={commands}
          conversation={conversation}
          empty={emptyText(silence)}
          front={front}
          notice={silence === undefined ? undefined : SILENCE_TEXT[silence].notice}
          onSend={onSend}
          ownSendCount={ownSendCount}
          sends={sends}
          placeholder={
            switchPending
              ? "Waiting for the session to answer the switch…"
              : `Message ${label} — delivered as ${MODE_LABELS[mode]}${commands === undefined ? "" : ", / for commands"}…`
          }
          replayCount={replays}
          resetKey={sessionId}
          sendDisabled={switchPending}
          sessionId={sessionId}
          stored={stored.data ?? []}
          viewer={viewer?.kind === "user" ? viewer.login : undefined}
        />
      </ErrorBoundary>
      {/* The one row under the message box (R6): what a message is delivered as, where the
          session runs, what it runs on, how full its window is, and the one question mark that
          holds every rule this page used to print. The facts scroll sideways at a phone width
          and wrap nowhere from `sm` up, so the row is one line on a laptop; the pickers hold
          their width (`shrink-0`, `SessionSettings`) and the directory is the one part that
          shortens. The ring and the question mark sit outside that scroller, so they keep the
          box's right edge however far the facts scroll, and their panels open upwards from
          here rather than inside a clipped scroller. */}
      <div
        className="mt-2 flex shrink-0 items-center gap-2 text-xs"
        data-testid="agent-composer-footer"
      >
        <div
          className="flex min-w-0 flex-1 flex-nowrap items-center gap-x-3 gap-y-1 overflow-x-auto sm:flex-wrap sm:overflow-x-visible"
          data-testid="agent-composer-facts"
        >
          <select
            aria-label="Delivery mode"
            className={`min-h-8 shrink-0 rounded-lg px-2 py-1 text-xs ${inputClasses(false)}`}
            data-testid="agent-delivery-picker"
            onChange={(event) => setPicked(event.target.value as MessageDeliveryMode)}
            value={mode}
          >
            {modes.map((candidate) => (
              <option key={candidate} value={candidate}>
                {MODE_LABELS[candidate]}
              </option>
            ))}
          </select>
          {/* Only a current list says the session is gone; one that failed or is refilling says
              nothing here, and the header already names why it has no list to read. */}
          {agent === undefined && current ? (
            <span
              className={`shrink-0 whitespace-nowrap ${textMutedOnCanvas}`}
              data-testid="agent-not-listed"
            >
              Not in the live session list — it may have ended.
            </span>
          ) : null}
          {agent === undefined ? null : (
            <>
              <span
                className={`shrink-0 whitespace-nowrap ${textMutedOnCanvas}`}
                data-testid="agent-footer-machine"
                title="The machine this session runs on"
              >
                {agent.host}
              </span>
              {/* The flex item the ellipsis applies to is the span inside, not this one
                  (`packages/dispatch/AGENTS.md`): a flex container never draws one on its own
                  text. `min-w-24` keeps a shortened directory readable, so the row scrolls at a
                  phone width rather than cutting it to nothing. */}
              <span
                className={`flex min-w-24 flex-1 items-center truncate ${textMutedOnCanvas}`}
                data-testid="agent-footer-dir"
              >
                <TruncatedText className="min-w-0 flex-1" title={agent.dir}>
                  {agent.dir}
                </TruncatedText>
              </span>
            </>
          )}
          <SessionSettings onPick={onPick} pending={switchPending} view={settings} />
        </div>
        <span className="flex shrink-0 items-center gap-1">
          <ContextRing
            onOpenChange={(next) => setPanel(next ? "context" : null)}
            open={panel === "context"}
            usage={conversation.context}
          />
          <MessageBoxHelp
            onOpenChange={(next) => setPanel(next ? "help" : null)}
            open={panel === "help"}
          />
        </span>
      </div>
      {/* Under the row, not in it: a line of prose in the row's sideways scroller shrinks to a
          sliver beside the pickers (`SessionSwitchLines`). */}
      <SessionSwitchLines notice={settings.notice} status={sessionSwitch.status} />
    </div>
  );
}
