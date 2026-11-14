import {
  ComposerPrimitive,
  MessagePrimitive,
  type ReasoningMessagePartComponent,
  type TextMessagePartComponent,
  ThreadPrimitive,
  type ToolCallMessagePartComponent,
  type Unstable_DirectiveFormatter,
  type Unstable_TriggerItem,
  type Unstable_TriggerMatcher,
  unstable_useTriggerPopoverScopeContext,
  useAui,
  useAuiState,
} from "@assistant-ui/react";
import type { AgentStreamCommand } from "@legion/contracts/agent-stream";
import {
  DELIVERY_CAPABILITIES,
  type MessageDeliveryMode,
} from "@legion/contracts/dispatch-constants";
import { type ReactNode, type RefObject, useEffect, useId, useMemo, useRef, useState } from "react";

import { ChevronIcon } from "../../components/DisclosureToggle";
import { ErrorBoundary } from "../../components/ErrorBoundary";
import { RefusableButton } from "../../components/RefusableButton";
import { TruncatedText } from "../../components/TruncatedText";
import {
  borderDefault,
  calloutDangerBorder,
  card,
  cardHoverBorder,
  dangerText,
  disclosureButtonText,
  inputClasses,
  primaryButtonBg,
  primaryButtonHoverBg,
  selectedCardBg,
  selectedCardBorder,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
  textOptionDescription,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { appendToDraft, DraftUploadStatus, useDraftUpload } from "../artifacts/ArtifactUpload";
import { MODE_LABELS } from "../conversation/delivery";
import { PendingSendTurn } from "../conversation/PendingSendTurn";
import type { SessionSends } from "../conversation/useSessionSends";
import { placeInView } from "../doc/readers-place";
import { AgentName } from "../refs/AgentName";
import { MarkdownBody } from "../refs/MarkdownBody";
import { readDispatchMarks, turnKey } from "./dispatch-marks";
import type { LivePlace } from "./live-views";
import { useLiveFollow } from "./use-live-follow";
import { useFrameCoalesced } from "./useFrameCoalesced";

/**
 * The session's conversation, rendered with assistant-ui's primitives (MIT) over Dispatch's own
 * theme classes rather than the library's stock styles, so it reads as part of the dashboard.
 * The runtime is supplied by the page above; this file is presentation only.
 */

const preClasses = `mt-2 max-h-64 overflow-auto rounded-md p-2 text-xs whitespace-pre-wrap ${surfaceMutedBg} ${textMutedOnSurfaceMuted}`;

/** A tool call and, once it lands, its result. Folded by default: tool traffic is the bulk of a
 *  session's output and a viewer is usually reading the prose around it. */
const ToolCall: ToolCallMessagePartComponent = ({ toolName, argsText, result, isError }) => {
  const [expanded, setExpanded] = useState(false);
  const failed = isError === true;
  const output = typeof result === "string" ? result : result === undefined ? "" : String(result);
  return (
    <div
      className={`my-1 rounded-lg border px-3 py-1 ${card} ${failed ? calloutDangerBorder : borderDefault}`}
      data-testid="agent-tool-call"
    >
      <button
        aria-expanded={expanded}
        className={`flex min-h-8 w-full items-center gap-2 text-left text-xs ${textSecondaryOnCanvas}`}
        onClick={() => setExpanded((open) => !open)}
        type="button"
      >
        <span className="font-semibold">{toolName}</span>
        {failed ? <span className={dangerText}>failed</span> : null}
        {result === undefined ? (
          <span className={textMutedOnCanvas}>running…</span>
        ) : (
          <span className={textMutedOnCanvas}>done</span>
        )}
        <span className={`ml-auto ${disclosureButtonText}`}>
          <ChevronIcon expanded={expanded} />
        </span>
      </button>
      {expanded ? (
        <div className="pb-2">
          <pre className={preClasses}>{argsText === "" ? "(no arguments)" : argsText}</pre>
          {output === "" ? null : (
            <pre className={preClasses} data-testid="agent-tool-result">
              {output}
            </pre>
          )}
        </div>
      ) : null}
    </div>
  );
};

/** A text part of a turn the session wrote, rendered as Markdown - re-parsed as the streamed
 *  text grows, so a heading or a list formats the moment its syntax closes; until then the
 *  partial syntax reads as the literal characters, at the surrounding text's size. A single
 *  newline is a line break here (`softBreaks="line"`): a model's prose puts its lines on lines
 *  rather than hard-wrapping a paragraph, and the document reading (one paragraph, joined by
 *  spaces) made `First, check the config.\nThen, verify the credentials.` one run-on line. Each
 *  re-parse reads the whole turn so far (about 3.6 ms per KB), so the text is read once per
 *  animation frame (`useFrameCoalesced`) rather than once per delta: the publisher sends at most
 *  ten snapshots a second, and a frame is the most anyone can see anyway. A module constant, as
 *  `Reasoning` is: a component minted per render would remount every part. */
const MarkdownText: TextMessagePartComponent = ({ text }) => (
  <MarkdownBody markdown={useFrameCoalesced(text)} softBreaks="line" />
);

const Reasoning: ReasoningMessagePartComponent = ({ text }) => (
  <div className={`my-1 text-xs italic ${textMutedOnCanvas}`} data-testid="agent-reasoning">
    <MarkdownBody markdown={useFrameCoalesced(text)} softBreaks="line" />
  </div>
);

/**
 * The key this turn keeps across a rebuild of the transcript, which every rendered turn carries in
 * `data-message-id` so `useLiveFollow` can find the turn a reader was on. One selector returning
 * the string, for the `Object.is` reason `AssistantMessage` documents below.
 */
function useTurnKey(): string {
  return useAuiState((state) => turnKey(state.message.metadata.custom, state.message.id));
}

function UserMessage(): ReactNode {
  // A message someone other than the viewer sent the session, stored or taken as its own turn
  // (AgentRuntimeThread names them): it sits on the session's side of the thread with its author,
  // never styled as the viewer's own. Both are text a person or an agent wrote (a broadcast, a
  // targeted message, a reply), so both render as Markdown, as the session's own turns do.
  // Two selectors, for the reason `AssistantMessage` gives: the `author` mark is a new object on
  // every rebuild of the transcript (each streamed frame), but its `actor` is the stored message's
  // own author, which keeps its identity across frames, and its `issue` is a string.
  const actor = useAuiState(
    (state) => readDispatchMarks(state.message.metadata.custom).author?.actor
  );
  const issue = useAuiState(
    (state) => readDispatchMarks(state.message.metadata.custom).author?.issue
  );
  const turn = useTurnKey();
  if (actor !== undefined) {
    return (
      <div
        className={`mt-4 max-w-[80%] rounded-xl border px-3 py-2 text-sm ${borderDefault} ${textPrimaryOnCanvas}`}
        data-message-id={turn}
        data-testid="agent-message-other"
      >
        <p className={`mb-1 text-xs font-semibold ${textSecondaryOnCanvas}`}>
          <AgentName actor={actor} inline />
          {issue === undefined ? null : ` · ${issue}`}
        </p>
        <MessagePrimitive.Parts components={{ Text: MarkdownText }} />
      </div>
    );
  }
  return (
    <div className="mt-4 flex justify-end" data-message-id={turn} data-testid="agent-message-user">
      <div
        className={`max-w-[80%] rounded-xl px-3 py-2 text-sm ${surfaceMutedBg} ${textPrimaryOnCanvas}`}
      >
        <MessagePrimitive.Parts components={{ Text: MarkdownText }} />
      </div>
    </div>
  );
}

function AssistantMessage(): ReactNode {
  // Two selectors, not one combined object read: `useAuiState` compares a selector's return by
  // `Object.is` and documents that returning a new object literal (as reading both marks into one
  // object here would) re-renders on every store update rather than only when the selected slice
  // changes (@assistant-ui/store's useAuiState.d.ts).
  // A reply the session sent through Dispatch (AgentRuntimeThread marks it), not a streamed turn.
  const fromDispatch = useAuiState(
    (state) => readDispatchMarks(state.message.metadata.custom).dispatch === true
  );
  // The provider/model that produced this streamed turn (LEGION-548): absent for a reply sent
  // through Dispatch, and for a turn whose publishing host reported none.
  const model = useAuiState((state) => readDispatchMarks(state.message.metadata.custom).model);
  // Whether the session is still writing this turn: assistant-ui's own message status, which
  // `toThreadMessages` sets to `running` while a message streams and to `complete` once a frame
  // settles it. A third selector returning a boolean, for the `Object.is` reason above. A turn
  // that calls a tool shows the tool card's own `running…`; this is the mark every other turn
  // has, including one that writes nothing but prose.
  const running = useAuiState((state) => state.message.status?.type === "running");
  const turn = useTurnKey();
  if (fromDispatch) {
    return (
      <div
        className={`mt-4 rounded-xl border px-3 py-2 text-sm ${card} ${borderDefault} ${textPrimaryOnCanvas}`}
        data-message-id={turn}
        data-testid="agent-dispatch-reply"
      >
        <p className={`mb-1 text-xs font-semibold ${textSecondaryOnCanvas}`}>Reply via Dispatch</p>
        <MessagePrimitive.Parts components={{ Text: MarkdownText }} />
      </div>
    );
  }
  return (
    <div
      className={`mt-4 text-sm ${textPrimaryOnCanvas}`}
      data-message-id={turn}
      data-testid="agent-message-assistant"
    >
      {model === undefined ? null : (
        <p
          className={`mb-1 truncate text-xs ${textMutedOnCanvas}`}
          data-testid="agent-message-model"
          title={model}
        >
          <TruncatedText>{model}</TruncatedText>
        </p>
      )}
      {running ? (
        <p className={`mb-1 text-xs ${textMutedOnCanvas}`} data-testid="agent-message-running">
          writing…
        </p>
      ) : null}
      <MessagePrimitive.Parts
        components={{ Reasoning, Text: MarkdownText, tools: { Fallback: ToolCall } }}
      />
    </div>
  );
}

/** The most commands the list shows at once: a session with every skill installed lists
 *  hundreds, and the person narrows them by typing. */
const SHOWN_COMMANDS = 50;

/** Where the `/name` token that starts at `offset` ends: at the first space after the slash,
 *  else at the end of the message. The span a mode pick drops. */
function commandNameEnd(text: string, offset: number, trigger: string): number {
  const nameEnd = text.slice(offset + trigger.length).search(/\s/u);
  return nameEnd === -1 ? text.length : offset + trigger.length + nameEnd;
}

/** How the session's publisher names a skill: `skill:<name>` (`AgentStreamCommand` in
 *  `packages/contracts/src/agent-stream.ts`; the plugin passes its host's picker names through).
 *  Anchored, so only the prefix goes and a name holding `skill:` later keeps it. */
const SKILL_PREFIX = /^skill:/u;

/**
 * The rows the list holds for `query`, in the order it shows them: the delivery modes lead the
 * session's own commands, since there are at most three of them and a session with every skill
 * installed lists hundreds. It is read in two places - the matcher below, which opens no empty
 * list, and the popover's adapter - so neither can offer what the other refuses, and it is
 * answered from `query` alone, which is the key the popover caches a search under.
 */
function slashListRows(
  commands: readonly AgentStreamCommand[],
  modes: readonly MessageDeliveryMode[],
  query: string
): Unstable_TriggerItem[] {
  return [...matchingModes(modes, query), ...matchingCommands(commands, query)];
}

/**
 * Where a slash command is being typed: the `/` token the caret sits in, whose `/` starts the
 * message or follows a space or a line break. A `/` inside a word - a path, a date - belongs to
 * that word and opens nothing. The replaced span is the whole token, so a pick made with the
 * caret inside a name replaces all of it.
 *
 * It also records where that `/` is, which is what the formatter below needs and the library
 * never hands it: `serialize(item)` takes the item alone. The matcher runs on every keystroke
 * and on every move of the caret, so the offset it leaves is the open popover's own.
 *
 * And only while the list has a row for what was typed. The popover is mounted whatever the
 * session advertises, so without this a session with no commands - or a query no mode and no
 * command matches - would open an empty "Slash commands" listbox: noise that says nothing and
 * may take the Enter meant for Send.
 */
function theSlashListMatcher(
  commands: readonly AgentStreamCommand[],
  modes: readonly MessageDeliveryMode[],
  triggerOffset: RefObject<number>
): Unstable_TriggerMatcher {
  return (text, trigger, cursor) => {
    // The token the caret sits in: back to the space or line break before it, or to the start.
    const offset = text.slice(0, cursor).search(/\S*$/u);
    if (!text.startsWith(trigger, offset)) return null;
    const query = text.slice(offset + trigger.length, cursor);
    if (slashListRows(commands, modes, query).length === 0) return null;
    const tokenEnd = text.slice(cursor).search(/\s/u);
    triggerOffset.current = offset;
    return {
      endOffset: tokenEnd === -1 ? text.length : cursor + tokenEnd,
      offset,
      query,
    };
  };
}

/**
 * What a picked command puts in the message. A pick that starts the message is a command line:
 * the session runs it as the person would have typed it at its terminal, `/skill:asd-ste100`,
 * and the popover adds the space after it. A pick inside a sentence is the person naming the
 * skill to the session - nothing there is run - so it writes the bare name, `asd-ste100`,
 * without the slash and without the publisher's `skill:` prefix.
 *
 * assistant-ui's own formatter writes a `:type[label]` directive, which the session would
 * receive as that markup rather than as its command. `parse` serves only a rich-text input,
 * which this composer is not.
 */
function slashFormatter(triggerOffset: RefObject<number>): Unstable_DirectiveFormatter {
  return {
    parse: (text) => [{ kind: "text", text }],
    serialize: (item) =>
      triggerOffset.current === 0 ? `/${item.id}` : item.id.replace(SKILL_PREFIX, ""),
  };
}

/** The prefix a mode row's id carries, so a session with a command of its own named `aside`
 *  still has a row the popover can tell from the mode's. */
const MODE_ITEM = "mode:";

/** The mode a row stands for, or undefined for one of the session's commands. */
function itemMode(item: Unstable_TriggerItem): MessageDeliveryMode | undefined {
  return DELIVERY_CAPABILITIES.find((mode) => mode === item.metadata?.mode);
}

/**
 * The delivery modes the list offers, in the order the "Send as" select has them and narrowed by
 * what the person typed after the slash. They lead the list: there are at most three of them, and
 * a session with every skill installed lists hundreds of commands behind them.
 */
function matchingModes(
  modes: readonly MessageDeliveryMode[],
  query: string
): Unstable_TriggerItem[] {
  const typed = query.toLowerCase();
  return modes
    .filter((mode) => MODE_LABELS[mode].toLowerCase().startsWith(typed))
    .map((mode) => ({
      id: `${MODE_ITEM}${mode}`,
      label: MODE_LABELS[mode],
      metadata: { mode },
      type: "delivery",
    }));
}

/**
 * The commands that match what the person typed after the slash, at most `SHOWN_COMMANDS`: every
 * command the session can run before the ones only its terminal runs (listed so the person learns
 * why `/new` does nothing here), and within each, names that start with what was typed before
 * names that merely hold it, otherwise in the session's own order.
 */
function matchingCommands(
  commands: readonly AgentStreamCommand[],
  query: string
): Unstable_TriggerItem[] {
  const typed = query.toLowerCase();
  const seen = new Set<string>();
  const ranked: { command: AgentStreamCommand; rank: number }[] = [];
  for (const command of commands) {
    // A name listed twice would be two rows with one id, which the popover cannot tell apart.
    if (seen.has(command.name)) continue;
    seen.add(command.name);
    const name = command.name.toLowerCase();
    if (!name.includes(typed)) continue;
    const rank = (command.terminalOnly === true ? 2 : 0) + (name.startsWith(typed) ? 0 : 1);
    ranked.push({ command, rank });
  }
  // `sort` is stable, so equal ranks keep the session's order.
  return ranked
    .sort((left, right) => left.rank - right.rank)
    .slice(0, SHOWN_COMMANDS)
    .map(({ command }) => ({
      description: command.description,
      id: command.name,
      label: `/${command.name}`,
      metadata: { terminalOnly: command.terminalOnly === true },
      type: command.source,
    }));
}

/** One row of the list: a mode's label, or a command's name and its description, with a muted
 *  note where the row is a delivery mode or a command only the session's terminal runs. The
 *  highlighted row stays in view as the arrows move through a list taller than the popover. */
function CommandItem({ index, item }: { index: number; item: Unstable_TriggerItem }): ReactNode {
  const highlighted = unstable_useTriggerPopoverScopeContext().highlightedIndex === index;
  const row = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    const highlightedRow = row.current;
    if (highlighted && highlightedRow !== null) placeInView(highlightedRow, { block: "nearest" });
  }, [highlighted]);
  return (
    // Below `xl` every button is `inline-flex` with centred items (styles.css's touch rule), so
    // the two lines stack explicitly and each stretches to the row's width.
    <ComposerPrimitive.Unstable_TriggerPopoverItem
      className={`flex min-h-11 w-full flex-col rounded-lg border px-3 py-2 text-left text-sm ${cardHoverBorder} ${
        highlighted ? `${selectedCardBg} ${selectedCardBorder}` : card
      }`}
      index={index}
      item={item}
      // Keeps the caret in the message, so the person types on after picking.
      onMouseDown={(event) => event.preventDefault()}
      ref={row}
    >
      <span className="flex items-baseline gap-2 self-stretch">
        <span className={`font-medium ${textPrimaryOnSurface}`}>{item.label}</span>
        {itemMode(item) !== undefined ? (
          <span className={`text-xs ${textOptionDescription}`}>delivery mode</span>
        ) : item.metadata?.terminalOnly === true ? (
          <span className={`text-xs ${textOptionDescription}`}>terminal only</span>
        ) : null}
      </span>
      {item.description === undefined ? null : (
        // Whole, on as many lines as it takes (INFRA-812, R5): a row of this list is read to
        // learn what the command does, and an ellipsis there hides the half that says it.
        <span className={`self-stretch text-xs ${textOptionDescription}`}>{item.description}</span>
      )}
    </ComposerPrimitive.Unstable_TriggerPopoverItem>
  );
}

/**
 * A mode pick writes nothing into the message: it is the "Send as" pick made from the list. The
 * popover takes one behaviour child, and that is the directive the session's own commands need,
 * so the mode rows take a selection override instead - which drops the `/name` the person
 * typed, exactly as an action's `removeOnExecute` would, and leaves whatever they had written
 * around it. The span it drops is the open popover's own `/` token, wherever in the message
 * that sits (`triggerOffset`, written by the matcher).
 */
function ModePicks({
  onPick,
  triggerOffset,
}: {
  onPick: (mode: MessageDeliveryMode) => void;
  triggerOffset: RefObject<number>;
}): ReactNode {
  const { registerSelectItemOverride, setCursorPosition } =
    unstable_useTriggerPopoverScopeContext();
  const aui = useAui();
  useEffect(
    () =>
      registerSelectItemOverride((item) => {
        const mode = itemMode(item);
        if (mode === undefined) return false;
        const text = aui.composer.getState().text;
        const offset = triggerOffset.current;
        const rest = text.slice(commandNameEnd(text, offset, "/"));
        aui.composer.setText(
          `${text.slice(0, offset)}${rest.startsWith(" ") ? rest.slice(1) : rest}`
        );
        setCursorPosition(offset);
        onPick(mode);
        return true;
      }),
    [aui, onPick, registerSelectItemOverride, setCursorPosition, triggerOffset]
  );
  return null;
}

/** The delivery modes the session takes and its own slash commands, offered for the `/` token
 *  the caret sits in. A mode pick sets "Send as" and writes nothing; a command pick writes the
 *  command at the start of a message and the bare name inside a sentence (`slashFormatter`),
 *  and sending it is the composer's ordinary Send. */
function SlashCommands({
  commands,
  modes,
  onPickMode,
}: {
  commands: readonly AgentStreamCommand[];
  modes: readonly MessageDeliveryMode[];
  onPickMode: (mode: MessageDeliveryMode) => void;
}): ReactNode {
  // Where the open popover's `/` sits, written by the matcher and read by the formatter and the
  // list, which the library gives nothing else to tell a command line from a mention by.
  const triggerOffset = useRef(0);
  const formatter = useMemo(() => slashFormatter(triggerOffset), []);
  const adapter = useMemo(
    () => ({
      categories: () => [],
      categoryItems: () => [],
      search: (query: string) => slashListRows(commands, modes, query),
    }),
    [commands, modes]
  );
  const matcher = useMemo(
    () => theSlashListMatcher(commands, modes, triggerOffset),
    [commands, modes]
  );
  return (
    <ComposerPrimitive.Unstable_TriggerPopover
      adapter={adapter}
      aria-label="Slash commands"
      char="/"
      className={`absolute right-0 bottom-full left-0 z-10 mb-2 flex max-h-72 flex-col gap-1 overflow-y-auto rounded-lg border p-1 shadow-lg ${card}`}
      matcher={matcher}
    >
      <ComposerPrimitive.Unstable_TriggerPopover.Directive formatter={formatter} />
      <ModePicks onPick={onPickMode} triggerOffset={triggerOffset} />
      <ComposerPrimitive.Unstable_TriggerPopoverItems className="contents">
        {(items) =>
          items.map((item, index) => <CommandItem index={index} item={item} key={item.id} />)
        }
      </ComposerPrimitive.Unstable_TriggerPopoverItems>
    </ComposerPrimitive.Unstable_TriggerPopover>
  );
}

/**
 * `resetKey` clears a caught error when the viewer moves to another session.
 *
 * The boundary is around the transcript alone, and deliberately not around the composer. A frame
 * this build renders wrongly throws inside assistant-ui's own conversion, and a boundary that
 * enclosed both would take the composer with it — leaving a viewer looking at an error with no
 * way to talk to the session, which is worse than the bad frame. The session's ring re-serves
 * that frame on every visit, so "worse" here means permanently.
 */
export function AgentThread({
  commands,
  contentIds,
  empty,
  front,
  held,
  initialPlace,
  modes,
  onPickMode,
  onPlace,
  ownSendCount,
  placeholder,
  replayCount,
  resetKey,
  sends,
  sessionId,
}: {
  /** The slash commands the composer completes, beside the delivery modes it always offers;
   *  no command is offered while this is undefined or empty. */
  commands: readonly AgentStreamCommand[] | undefined;
  /** Every turn of the transcript, in the order it is rendered, by the key each one carries in
   *  `data-message-id`: what the follow hook counts as new and finds the reader's place in. */
  contentIds: readonly string[];
  empty: string;
  /** Whether this is the view the person is looking at. A view the shell keeps mounted behind
   *  the one in front is laid out over the page rather than in it, so its own geometry is the
   *  shell's doing and never the reader moving (`useLiveFollow`). */
  front: boolean;
  /** Whether a model or thinking switch is pending on this session: Send is disabled, still
   *  reading "Send", until it ends, and `placeholder` is what says why. */
  held: boolean;
  /** Where this session's reader was when its view last reported, which the view's entry in
   *  `live-views.ts` holds while the view sleeps, or `null` for one that was following. */
  initialPlace: LivePlace | null;
  /** The delivery modes this session takes, in the order the row under the message box has
   *  them: the `/` list offers each of them. */
  modes: readonly MessageDeliveryMode[];
  /** Picks the mode the next message goes in, as that row's own picker does. */
  onPickMode: (mode: MessageDeliveryMode) => void;
  /** Reports the place the transcript holds, for that entry to keep. */
  onPlace: (place: LivePlace | null) => void;
  /** Bumped once per message this viewer sent that the session accepted. */
  ownSendCount: number;
  placeholder: string;
  /** Bumped once per rebuild of the whole transcript from the session's replay. */
  replayCount: number;
  resetKey: string;
  /** Every message to the session Dispatch has not yet made, in press order, each a turn of its
   *  own at the transcript's end, and what acts on them. */
  sends: SessionSends;
  /** The session being read: what an upload from this composer belongs to. */
  sessionId: string;
}): ReactNode {
  // The element itself, not a ref held once: this viewport is inside the boundary below, which
  // renders a fresh node once a reset clears the error it was holding, and the hook binds its
  // listener and its observers to whichever node it is given.
  const [viewport, setViewport] = useState<HTMLDivElement | null>(null);
  const { pending } = sends;
  const follow = useLiveFollow({
    contentIds: useMemo(
      () => [...contentIds, ...pending.map((send) => `pending:${send.id}`)],
      [contentIds, pending]
    ),
    front,
    initialPlace,
    onPlace,
    ownSendCount,
    replayCount,
    viewport,
  });
  // An empty box is the one thing that refuses Send (INFRA-442): a message still out, late or
  // refused is a turn of its own, and the next message goes out behind it. A pending switch
  // disables Send and names why instead. A turn still streaming does not refuse: the session
  // decides what a message does mid-turn by its delivery mode, as at its terminal.
  const boxEmpty = useAuiState((state) => state.composer.isEmpty);
  const refusal = boxEmpty && !held ? "Type a message first." : null;
  // The line under the box reads nothing until a person presses Send and the press is refused
  // (INFRA-812, R6): an empty box is where this view rests, and a reason printed there before
  // anybody tries is rule text on the page, which the question mark under the box now holds.
  // The reason reaches a screen reader all the same, on the button's own hidden line and its
  // title, and the next thing typed clears the press.
  const [refusedPress, setRefusedPress] = useState(false);
  const shownRefusal = refusedPress ? refusal : null;
  if (refusal === null && refusedPress) setRefusedPress(false);
  const hintId = useId();
  // A file pasted or dropped into the composer is the session's own artifact, as one pasted into
  // a direct message on the Agents page is, and its line joins the draft rather than replacing
  // it: the person keeps what they had typed.
  const aui = useAui();
  const upload = useDraftUpload((text) =>
    aui.composer.setText(appendToDraft(aui.composer.getState().text, text))
  );
  const uploadOwner = useMemo(() => ({ session: sessionId }), [sessionId]);
  // The picture is in the draft only once the upload lands, so a message sent before that would
  // go without it and the picture's line would join whatever is typed next. Send says the upload
  // is what holds it and the form refuses the send under it, so neither way out of this box
  // sends a message the picture would miss - the rule every upload host keeps
  // (`packages/dispatch/AGENTS.md`, `MentionComposer`, `AskAnswerArea`).
  const uploading = upload.pending > 0;
  return (
    <ThreadPrimitive.Root className="flex min-h-0 flex-1 flex-col">
      <ErrorBoundary region="this conversation" resetKey={resetKey}>
        {/* `relative` for the control below, which is the viewport's sibling and out of flow, so
            it changes neither `scrollHeight` nor `scrollTop`, the composer does not move, and it
            can never cover the composer's controls. */}
        <div className="relative flex min-h-0 flex-1 flex-col">
          {/* `mt-3`: the scroller clips a turn at its top edge, and without a gap that clipped
              line sat against the page's note above it and read as overlapping text. `pl-1`
              matches `pr-1` for the same reason on the left: a turn's numbered list hangs its
              marker outside the list's content box, and the scroller clipped its first digit.

              All four of assistant-ui's auto-scroll options are off and `useLiveFollow` does
              every scroll this view makes: with them off the library plants no scroll intent and
              performs none. Its own `thread.runStart` handler scrolls to the bottom without
              asking where the reader is, which is this view's whole complaint; its scroll-up
              detector requires `scrollHeight` to be unchanged, so it never notices a reader who
              scrolls back while a turn streams; and the flag it would then keep is internal, so
              nothing outside the library can clear it.

              `[overflow-anchor:none]` (the sidebar's precedent in `app.tsx`) leaves the hook's
              own compensation the only one there is: the browser's anchoring would adjust a
              rebuild a frame after the hook already had, hiding that compensation and arriving
              as a scroll event nothing can tell from the reader's own. */}
          <ThreadPrimitive.Viewport
            autoScroll={false}
            className="mt-3 min-h-0 flex-1 overflow-y-auto pr-1 pl-1 [overflow-anchor:none]"
            data-testid="agent-thread"
            ref={setViewport}
            scrollToBottomOnInitialize={false}
            scrollToBottomOnRunStart={false}
            scrollToBottomOnThreadSwitch={false}
          >
            {/* One wrapper around everything the viewport scrolls: the hook's `ResizeObserver`
                measures the viewport's first element child as the transcript's height. */}
            <div>
              <ThreadPrimitive.Empty>
                {/* Why there is nothing here is the page's to say: a session that cannot stream
                    never fills this space, and "the next turn appears here" would be a
                    promise. */}
                <p className={`mt-6 text-sm ${textMutedOnCanvas}`} data-testid="agent-thread-empty">
                  {empty}
                </p>
              </ThreadPrimitive.Empty>
              <ThreadPrimitive.Messages components={{ AssistantMessage, UserMessage }} />
              {pending.map((send) => (
                <PendingSendTurn held={send} key={send.id} sends={sends} />
              ))}
            </div>
          </ThreadPrimitive.Viewport>
          {follow.atEnd ? null : (
            <button
              className={`absolute bottom-3 left-1/2 z-10 min-h-11 -translate-x-1/2 rounded-full px-4 text-sm font-semibold whitespace-nowrap shadow-lg ${primaryButtonBg} ${primaryButtonHoverBg}`}
              data-testid="jump-to-latest"
              onClick={follow.jumpToEnd}
              type="button"
            >
              Jump to latest{follow.newCount === 0 ? "" : ` · ${follow.newCount} new`}
            </button>
          )}
        </div>
      </ErrorBoundary>
      {/* The trigger root always wraps the composer, so commands arriving or going never
          remounts the input under the person's caret. The form stays the composer's one box,
          `relative`, so the command list is anchored to it and the whole composer, hint line
          included, is what a layout measures. */}
      <ComposerPrimitive.Unstable_TriggerPopoverRoot>
        <ComposerPrimitive.Root
          className={`relative mt-3 flex flex-col gap-1 border-t pt-3 ${borderDefault}`}
          data-testid="agent-composer"
          // The input is assistant-ui's own, so the paste and the drop are taken here, where
          // they bubble to: a file goes to the session's artifacts in place of the paste, and
          // the text the browser would have pasted never reaches the box. `onDragOver` is what
          // makes the form a drop target at all - without it the browser opens the file.
          onDragOver={(event) => event.preventDefault()}
          onDrop={(event) => upload.drop(event, uploadOwner)}
          onPaste={(event) => upload.paste(event, uploadOwner)}
          // Where both ways out of this box arrive - the button's press and Enter, which
          // assistant-ui's input turns into this form's own submit - so this is the one place a
          // picture still uploading holds the message: the composer root runs its send only
          // where this handler has not prevented the default (`composeEventHandlers`).
          onSubmit={(event) => {
            if (uploading) event.preventDefault();
          }}
        >
          <SlashCommands commands={commands ?? []} modes={modes} onPickMode={onPickMode} />
          <div className="flex items-end gap-2">
            <ComposerPrimitive.Input
              className={`min-h-11 flex-1 rounded-lg px-3 py-2 text-sm ${inputClasses(false)}`}
              placeholder={placeholder}
              rows={1}
              // Enter stays the send's key while a picture uploads: taking it off the box would
              // hand the reader a stray line break in their message instead of the reason the
              // send waits, which the form's own refusal above gives them (`onSubmit`).
              submitMode="enter"
            />
            {/* A submit button, so a click and Enter both reach the form's submit, which the
                composer root turns into assistant-ui's own send: one way out for a message. */}
            <RefusableButton
              busy={held ? "Send" : uploading ? "Uploading file…" : undefined}
              className="min-h-11"
              describedBy={held ? hintId : undefined}
              onRefusedPress={() => setRefusedPress(true)}
              refusal={refusal}
              refusalShownBy={shownRefusal === null ? undefined : hintId}
              type="submit"
            >
              Send
            </RefusableButton>
          </div>
          {/* An upload still out, and the reason one was refused with its Retry, under the box
              the file was dropped into. */}
          <DraftUploadStatus upload={upload} />
          {/* The Send control's own words, and nothing else: while a switch is pending this line
              says what the box's placeholder says, which the text typed may hide, and once a
              person has pressed Send with nothing typed it says why that press was refused. At
              rest it reads nothing - how to send is a rule of the page, which the question mark
              under the box now holds (INFRA-812, R6), not a word about this press. */}
          <p
            className={`text-xs ${textMutedOnCanvas}`}
            data-testid="agent-composer-hint"
            id={hintId}
          >
            {held ? placeholder : shownRefusal}
          </p>
        </ComposerPrimitive.Root>
      </ComposerPrimitive.Unstable_TriggerPopoverRoot>
    </ThreadPrimitive.Root>
  );
}
