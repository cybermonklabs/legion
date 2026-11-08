import { useQuery } from "@tanstack/react-query";
import {
  type ClipboardEvent,
  type DragEvent,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
  type SyntheticEvent,
  useCallback,
  useEffect,
  useId,
  useImperativeHandle,
  useMemo,
  useRef,
  useState,
} from "react";

import { issueCollaboratorsQuery } from "../../api/queries";
import type { Agent, IssueCollaboratorAgent, IssueCollaboratorLegionAgent } from "../../api/types";
import { AgentListRefilling } from "../../components/QueryError";
import { TruncatedText } from "../../components/TruncatedText";
import {
  borderDefault,
  dangerText,
  inputClasses,
  selectedCardBorder,
  surfaceMutedHoverBg,
  surfaceRecessedBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AgentName, type AgentText, useAgentText } from "../refs/AgentName";
import { relativeTimeLabel, Timestamp, useRelativeTimeTick } from "../refs/Timestamp";
import type { TextEditRange } from "./block-spans";
import type { AcceptedMention } from "./composer-model";
import { mentionText, survivingMentions } from "./send-request";
import { useAgents } from "./useAgents";

export interface MentionOption {
  readonly capabilities: readonly string[];
  /** The detail line under the option's name, which its filter matches too: a running session's
   *  directory, or, for a role nobody is running, the role and that it is not running. */
  readonly detail: string;
  /** The name of the session the option is, or of the one holding its role, as `useAgentText`
   *  gives it: the filter matches it, and a role option's detail tooltip names it. */
  readonly holder: string;
  /** When the session was last seen, ISO (`Agent.last_seen` is epoch milliseconds): the picker
   *  shows it as a relative `<Timestamp>`, the exact time on its `title`. Undefined for an
   *  option no session is running, which has no such time and shows none. */
  readonly lastSeen?: string;
  /** The session the option is, or, for a role, the session holding it (the newest one for a
   *  role nobody holds): `AgentName` draws it. */
  readonly sessionId: string;
  /** That session's title as the agent list gave it: its name when the registry cache has none. */
  readonly sessionTitle: string;
  readonly target: string;
  /** The text the mention inserts (`@Title`) and the option's accessible name: a role's name, or
   *  the session's name as `useAgentText` gives it. */
  readonly title: string;
}

function roleOptions(
  agents: readonly Agent[],
  text: Pick<AgentText, "sessionText">
): MentionOption[] {
  const byRole = new Map<string, Agent>();
  for (const agent of agents) {
    for (const role of agent.roles) byRole.set(role, agent);
  }
  return [...byRole.entries()]
    .sort(([left], [right]) => {
      const leftController = left.includes("controller");
      const rightController = right.includes("controller");
      return leftController === rightController
        ? left.localeCompare(right)
        : leftController
          ? -1
          : 1;
    })
    .map(([role, agent]) => ({
      capabilities: agent.capabilities,
      detail: agent.dir,
      holder: text.sessionText(agent.session_id, agent.title),
      lastSeen: new Date(agent.last_seen).toISOString(),
      sessionId: agent.session_id,
      sessionTitle: agent.title,
      target: `role:${role}`,
      title: role,
    }));
}

export function mentionOptions(
  agents: readonly Agent[],
  text: Pick<AgentText, "sessionText">
): MentionOption[] {
  return [
    ...roleOptions(agents, text),
    ...[...agents]
      .sort(
        (left, right) => right.last_seen - left.last_seen || left.title.localeCompare(right.title)
      )
      .map((agent) => ({
        capabilities: agent.capabilities,
        detail: agent.dir,
        holder: text.sessionText(agent.session_id, agent.title),
        lastSeen: new Date(agent.last_seen).toISOString(),
        sessionId: agent.session_id,
        sessionTitle: agent.title,
        target: `session:${agent.session_id}`,
        title: text.sessionText(agent.session_id, agent.title),
      })),
  ];
}

/**
 * The picker's "Not running" options: one per Legion role token the issue's collaborators record
 * (`IssueCollaborators.agents[].legion`) that no live agent holds - the live agents' `roles`, the
 * rule the live role options follow - addressed `role:<token>` and drawn by the role's newest
 * session, the one entry per token the server leaves unreplaced (`legion.replaced_by` null), so a
 * replaced session is never an option of its own. A session the live list itself names is left
 * out whatever its roles say: a live session that claims no role would otherwise be offered here
 * and draw its own `Running` mark, so one row would read running and not running at once.
 *
 * The options keep the server's order, which the collaborators contract states: the registry's
 * agents by role in workflow order, then newest first.
 */
export function stoppedRoleOptions(
  live: readonly Agent[],
  collaborators: readonly IssueCollaboratorAgent[],
  text: Pick<AgentText, "sessionText">
): MentionOption[] {
  const held = new Set(live.flatMap((agent) => agent.roles));
  const running = new Set(live.map((agent) => agent.session_id));
  const offered = (agent: IssueCollaboratorAgent): agent is IssueCollaboratorLegionAgent =>
    agent.legion !== null &&
    agent.legion.replaced_by === null &&
    !held.has(agent.legion.role_token) &&
    !running.has(agent.session_id);
  return collaborators.filter(offered).map((agent) => {
    // The title the server gives is the registry's, else the session's newest stamped one; the
    // name is still `useAgentText`'s, as a live option's holder is.
    const storedTitle = agent.title ?? "";
    return {
      capabilities: [],
      detail: `${agent.legion.role} · not running`,
      holder: text.sessionText(agent.session_id, storedTitle),
      sessionId: agent.session_id,
      sessionTitle: storedTitle,
      target: `role:${agent.legion.role_token}`,
      title: agent.legion.role_token,
    };
  });
}

/** Where the agent list the picker offers stands: `current` is a read that landed, and the
 *  three others are a list the picker must not speak for. A picker whose list is not current
 *  never says nobody matches - it says what is true of the read instead, so a refilling or
 *  unreachable listener never reads as an empty registry (`AgentListRefilling`). A host that
 *  supplies its own agents is always current: that list is its own answer. */
export type MentionListing = "current" | "loading" | "refilling" | "unreachable";

/**
 * Everything a host of this field has to wire up to give it a working `@` picker: the option
 * list, where the read that gave it stands, whether the picker is open, and the report the
 * field sends when that changes. Both hosts take it from here - the Conversation tab's composer
 * (`MentionComposer.tsx`) and an ask's answer area (`features/inbox/AskAnswerArea.tsx`) -
 * because they had written the same four steps twice and the copies had already drifted: only
 * the composer read the registry again when its picker opened, so a session started inside the
 * poller's 15 s was offered in one picker and not the other.
 *
 * `enabled` is whether this host's channel takes a mention at all. `suppliedAgents`, where a
 * host has its own list, is offered in place of the registry and no registry read is made.
 * `issueKey` is the issue whose Legion roles the "Not running" options come from; a host with
 * no issue of its own offers none.
 */
export function useMentionOptions(
  enabled: boolean,
  suppliedAgents?: readonly Agent[],
  issueKey?: string
): {
  /** For the field's `listing`. */
  readonly listing: MentionListing;
  /** For the field's `onPickerOpenChange`. */
  readonly onPickerOpenChange: (open: boolean) => void;
  readonly options: MentionOption[];
  /** Whether the picker is open, for a host that moves under it: the docked composer rises over
   *  the page while the list is showing. */
  readonly pickerOpen: boolean;
  /** For the field's `stoppedOptions`: the issue's Legion roles no live agent holds. */
  readonly stoppedOptions: MentionOption[];
} {
  const [pickerOpen, setPickerOpen] = useState(false);
  const readsTheRegistry = suppliedAgents === undefined && enabled;
  const live = useAgents(readsTheRegistry, pickerOpen);
  const agents = suppliedAgents ?? live.agents;
  const text = useAgentText();
  const options = useMemo(() => mentionOptions(agents, text), [agents, text]);
  const listing: MentionListing = !readsTheRegistry
    ? "current"
    : live.current
      ? "current"
      : live.refilling
        ? "refilling"
        : live.isPending
          ? "loading"
          : "unreachable";
  // The issue's collaborators feed the "Not running" options, read once the picker opens and
  // only where the host named an issue: a project document's margin composer carries that
  // document rather than an issue, while an issue document's margin composer is an issue owner
  // and does read them, and a closed picker and a direct message read nothing. The query is the
  // Collaborators tab's, so the two readers share one entry - a tab read within the shell's 30 s
  // `staleTime` opens the picker with no request of its own - and the event feed refreshes that
  // key by a rule of its own (`changesCollaborators` in event-refresh.ts), which refetches it
  // under a picker that stays open.
  const collaborators = useQuery({
    ...issueCollaboratorsQuery(issueKey),
    enabled: issueKey !== undefined && enabled && pickerOpen,
  });
  // A live list whose state is unknown (not read yet, or its read failed) holds every role as
  // far as the picker can tell, which is what a `listing` other than `current` says of it, so no
  // role is offered as not running.
  const stoppedOptions = useMemo(
    () =>
      listing === "current"
        ? stoppedRoleOptions(agents, collaborators.data?.agents ?? [], text)
        : [],
    [agents, collaborators.data, listing, text]
  );
  const { refetch } = live;
  useEffect(() => {
    // An open picker is kept fresh through the shell's one poller (`useAgents`), whose interval
    // is 15 s, and read once more here the moment it opens: a session started since the last
    // poll is offered to the reader who is asking for it now.
    if (readsTheRegistry && pickerOpen) {
      void refetch();
    }
  }, [pickerOpen, readsTheRegistry, refetch]);
  return { listing, onPickerOpenChange: setPickerOpen, options, pickerOpen, stoppedOptions };
}

/** Reconcile accepted mentions against the actual textarea edit range. */
export function reconcileMentions(
  before: string,
  after: string,
  mentions: readonly AcceptedMention[],
  edit?: TextEditRange
): AcceptedMention[] {
  if (edit !== undefined) {
    const delta = after.length - before.length;
    return mentions.flatMap((mention) => {
      if (mention.end <= edit.start) return [mention];
      if (mention.start >= edit.end) {
        return [{ ...mention, end: mention.end + delta, start: mention.start + delta }];
      }
      return [];
    });
  }
  const delta = after.length - before.length;
  if (delta < 0) {
    const deleted = -delta;
    const possibleStarts = Array.from({ length: after.length + 1 }, (_, start) => start).filter(
      (start) =>
        before.slice(0, start) === after.slice(0, start) &&
        before.slice(start + deleted) === after.slice(start)
    );
    return mentions.flatMap((mention) => {
      if (possibleStarts.some((start) => mention.start < start + deleted && mention.end > start)) {
        return [];
      }
      const shifts = new Set(possibleStarts.map((start) => (start < mention.start ? delta : 0)));
      if (shifts.size !== 1) return [];
      const shift = shifts.values().next().value;
      if (shift === undefined) return [];
      return [{ ...mention, end: mention.end + shift, start: mention.start + shift }];
    });
  }
  let prefix = 0;
  const common = Math.min(before.length, after.length);
  while (prefix < common && before[prefix] === after[prefix]) prefix += 1;
  return mentions.flatMap((mention) => {
    if (mention.end <= prefix) return [mention];
    if (mention.start >= prefix) {
      return [{ ...mention, end: mention.end + delta, start: mention.start + delta }];
    }
    return [];
  });
}

function mentionQuery(
  body: string,
  selectionStart: number | null
): { query: string; start: number } | undefined {
  const caret = selectionStart ?? body.length;
  const before = body.slice(0, caret);
  const at = before.lastIndexOf("@");
  if (at < 0 || /\s/.test(before.slice(at + 1))) return undefined;
  return { query: before.slice(at + 1), start: at };
}

export function mentionDisplay(title: string, target: string): string {
  const trimmed = title.trim();
  if (trimmed !== "" && !/^(?:session|role):/i.test(trimmed) && !/[@\r\n]/.test(trimmed)) {
    return trimmed;
  }
  return target.replace(":", " ");
}

/** One edit of the field: the text now, the accepted mentions reconciled against it, and the
 *  edit the browser named, which a host following block spans maps them through (`followEdit`).
 *  Both arrive together because a mention's offsets are into that exact text. */
export interface MentionFieldChange {
  readonly body: string;
  readonly inputType?: string;
  readonly mentions: AcceptedMention[];
  readonly range?: TextEditRange;
}

/** What a host reaches the field it mounted by, through the `fieldRef` it passed. */
export interface MentionFieldHandle {
  /** Closes the `@` picker, leaving the draft alone: a send does, since the words the picker
   *  stood over are on the wire. */
  readonly closePicker: () => void;
  /** The field's textarea as it stands, null while the field is unmounted. A host reads what the
   *  reader typed from here rather than from the value React last rendered - a keystroke can land
   *  between that render and a press of Send - and moves the focus into it. */
  readonly textarea: () => HTMLTextAreaElement | null;
}

/**
 * The body field of a draft and its `@` picker: one textarea, the mentions accepted in it, and
 * the listbox of agents and roles that `@` opens. The host holds the draft - the text and the
 * accepted mentions reach it through `onChange`, and come back as props - because the host is
 * what sends it; the field holds only what the picker is showing. Two hosts mount it: the
 * Conversation tab's composer (`MentionComposer.tsx`) and an ask's answer area
 * (`features/inbox/AskAnswerArea.tsx`), so both give one picker with one set of rules.
 *
 * While the picker is open it owns ArrowDown, ArrowUp, Enter and Escape, and none of the four
 * reaches anything around the field: the Inbox binds the arrows and Enter as its own hotkeys
 * (`features/inbox/Inbox.tsx`), and a card or a dialog closes on Escape. With the picker closed
 * every key is the host's, as it always was.
 */
export function MentionField({
  body,
  disabled = false,
  fieldRef,
  handle,
  inline = false,
  label,
  labelHidden = false,
  listing = "current",
  mentions,
  onChange,
  onDrop,
  onKeyDown,
  onPaste,
  onPickerOpenChange,
  options,
  stoppedOptions,
  placeholder,
}: {
  readonly body: string;
  /** Takes no typing: a draft whose own send is out, which the host shows the reader rather than
   *  letting them write over what is already on the wire (`AskAnswerArea`). */
  readonly disabled?: boolean;
  readonly fieldRef: RefObject<MentionFieldHandle | null>;
  /** A bare `data-` attribute written on the textarea itself, the one handle a host outside the
   *  draft reaches the box by: the Inbox's answer keys find the selected ask's answer box by
   *  `data-ask-answer` and move the focus into it (`features/inbox/Inbox.tsx`). */
  readonly handle?: string;
  /** A one-line field that grows with what is typed, as a reply's does. */
  readonly inline?: boolean;
  /** The field's accessible name, shown above it unless `labelHidden`. */
  readonly label: string;
  readonly labelHidden?: boolean;
  /** Where the read that gave `options` stands (`useMentionOptions`). A picker whose list is
   *  not current says so in place of "No live agents match.", which only a current list may
   *  claim. */
  readonly listing?: MentionListing;
  /** The mentions accepted in `body`, whose offsets are into that exact text. */
  readonly mentions: readonly AcceptedMention[];
  readonly onChange: (change: MentionFieldChange) => void;
  readonly onDrop?: (event: DragEvent<HTMLTextAreaElement>) => void;
  /** The host's own keys - a submit on Ctrl/Cmd+Enter, a picker on Ctrl/Cmd+K - which an open
   *  picker's four keys never reach. */
  readonly onKeyDown?: (event: KeyboardEvent<HTMLTextAreaElement>) => void;
  readonly onPaste?: (event: ClipboardEvent<HTMLTextAreaElement>) => void;
  /** Told whenever the picker opens or closes: a host refreshes the agent registry while it is
   *  open, and lifts itself over whatever it floats above. */
  readonly onPickerOpenChange?: (open: boolean) => void;
  /** The agents and roles `@` offers (`mentionOptions`). Undefined where the channel takes no
   *  mention at all - an edit, or a message that already reaches its one session - and the field
   *  is then a plain textarea; empty while the registry names nobody, and the picker says so. */
  readonly options?: readonly MentionOption[];
  /** What the empty box says it takes, where the host has words for it (an ask's answer box
   *  names what the picked option asks of the reader). */
  readonly placeholder?: string;
  /** The roles the host's issue records that nobody is running (`stoppedRoleOptions`), offered
   *  under "Not running" after the live options. Undefined where the host has no issue to read
   *  them from, which is every host but the issue's own composer. */
  readonly stoppedOptions?: readonly MentionOption[];
}): ReactNode {
  const element = useRef<HTMLTextAreaElement | null>(null);
  const [autocomplete, setAutocomplete] = useState<{ query: string; start: number }>();
  /** Which option the arrows are on, none until a key moves it: the first ArrowDown takes the
   *  first option, so a reader who presses Enter straight away picks nothing by accident. */
  const [active, setActive] = useState<number>();
  // What the field last showed - its text and the records in step with it - which the next edit
  // is measured from. It is the host's own state, which the render below brings back, plus
  // whatever an edit in this same task has already made of it, since the host's state is a
  // render behind that. A draft the host puts in the field (a held send's, an upload's text, a
  // reference) arrives the same way, so the next edit is measured from that instead.
  const previousBody = useRef(body);
  previousBody.current = body;
  const shownMentions = useRef(mentions);
  shownMentions.current = mentions;
  const pendingTextEdit = useRef<
    { before: string; inputType: string; range: TextEditRange } | undefined
  >(undefined);
  const listboxId = useId();
  const notRunningId = useId();
  const open = autocomplete !== undefined;
  // The detail line below ends in that session's last-seen time, drawn as the `<Timestamp>`
  // every other relative time on the page is, and the whole line is also that line's `title`:
  // it is cut to the row's width, and a pointer reader hovers it for what was cut. The title is
  // a string, so it is built here rather than rendered, and this subscribes to the one tick
  // `<Timestamp>` follows so the two say the same thing for as long as the list is open.
  useRelativeTimeTick(open);
  const [filteredOptions, filteredStoppedOptions] = useMemo<
    [readonly MentionOption[], readonly MentionOption[]]
  >(() => {
    if (autocomplete === undefined || options === undefined) return [[], []];
    const query = autocomplete.query.toLocaleLowerCase();
    // A role option matches its holder's name too, which its detail line shows, and either kind
    // matches the session's folder: the time beside it in that line is no search term.
    const matches = (option: MentionOption) =>
      query === "" ||
      option.target.toLocaleLowerCase().includes(query) ||
      option.title.toLocaleLowerCase().includes(query) ||
      option.holder.toLocaleLowerCase().includes(query) ||
      option.detail.toLocaleLowerCase().includes(query);
    return [options.filter(matches), (stoppedOptions ?? []).filter(matches)];
  }, [autocomplete, options, stoppedOptions]);
  /** Every option the arrows step through, in the order the list draws them: the live ones,
   *  then the roles nobody is running. The index is the option's place in this list, which is
   *  also the id each row carries. */
  const navigableOptions = useMemo(
    () => [...filteredOptions, ...filteredStoppedOptions],
    [filteredOptions, filteredStoppedOptions]
  );
  // The list is read again while the picker is open, so an option the reader was on can go.
  const activeOption = active === undefined ? undefined : navigableOptions[active];
  /** The option the field reports as active, which is the one Enter picks. */
  const activeId = activeOption === undefined ? undefined : `${listboxId}-${active}`;
  const activeMentions = useMemo(() => survivingMentions(body, mentions), [body, mentions]);

  const closePicker = useCallback(() => {
    setAutocomplete(undefined);
    setActive(undefined);
  }, []);
  useImperativeHandle(fieldRef, () => ({ closePicker, textarea: () => element.current }), [
    closePicker,
  ]);
  useEffect(() => {
    onPickerOpenChange?.(open);
  }, [onPickerOpenChange, open]);

  const addMention = (option: MentionOption) => {
    const current = element.current?.value ?? body;
    const query = mentionQuery(current, element.current?.selectionStart ?? null);
    if (
      query === undefined ||
      shownMentions.current.some((mention) => mention.target === option.target)
    ) {
      closePicker();
      return;
    }
    const display = mentionDisplay(option.title, option.target);
    const text = mentionText(display);
    const caret = element.current?.selectionStart ?? current.length;
    const after = `${current.slice(0, query.start)}${text}${current.slice(caret)}`;
    const accepted = [
      ...reconcileMentions(current, after, shownMentions.current, {
        end: caret,
        start: query.start,
      }),
      { end: query.start + text.length, start: query.start, target: option.target, text: display },
    ];
    previousBody.current = after;
    shownMentions.current = accepted;
    onChange({ body: after, mentions: accepted });
    closePicker();
    requestAnimationFrame(() => {
      element.current?.focus();
      element.current?.setSelectionRange(query.start + text.length, query.start + text.length);
    });
  };
  const changed = (value: string, selectionStart: number | null) => {
    const before = previousBody.current;
    const pending = pendingTextEdit.current;
    pendingTextEdit.current = undefined;
    const accepted = reconcileMentions(
      before,
      value,
      shownMentions.current,
      pending?.before === before ? pending.range : undefined
    );
    previousBody.current = value;
    shownMentions.current = accepted;
    onChange({
      body: value,
      inputType: pending?.inputType,
      mentions: accepted,
      range: pending?.range,
    });
    if (options === undefined) {
      closePicker();
      return;
    }
    setAutocomplete(mentionQuery(value, selectionStart));
    setActive(undefined);
  };
  /** The keys an open picker owns, on a listener of the field's own. React's handlers run when
   *  the event reaches the root, by which time it has passed every host between - the Inbox's
   *  window-bound hotkeys, a card's or a dialog's Escape - so stopping it there would stop
   *  nothing: only a listener on the field itself is in front of them. It is written on every
   *  render and read through a ref, so it sees the picker as it stands without being re-bound. */
  const pickerKey = useRef<(event: globalThis.KeyboardEvent) => void>(() => {});
  pickerKey.current = (event) => {
    if (!open) return;
    if (event.key === "Escape") {
      // Only the picker closes: not a dialog, not a card, not the composer around it.
      event.stopPropagation();
      closePicker();
      return;
    }
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      event.stopPropagation();
      const rows = navigableOptions.length;
      if (rows === 0) return;
      const step = event.key === "ArrowDown" ? 1 : -1;
      // Nothing active yet: down takes the first option, up the last. Either wraps from there.
      setActive(active === undefined ? (step === 1 ? 0 : rows - 1) : (active + step + rows) % rows);
      return;
    }
    // A modified Enter is not the picker's: Ctrl/Cmd+Enter is the host's submit and Shift+Enter
    // the reader's newline, picker or no picker. A plain one belongs to the picker while it is
    // open, and types its newline only where no option is active to take it.
    if (
      event.key === "Enter" &&
      !event.ctrlKey &&
      !event.metaKey &&
      !event.altKey &&
      !event.shiftKey
    ) {
      event.stopPropagation();
      if (activeOption === undefined) return;
      event.preventDefault();
      addMention(activeOption);
    }
  };
  // The body field records each edit from the browser's own `beforeinput`: its type, which tells
  // `mapSpans` a cut or a paste, and the range it replaces. React's `onBeforeInput` is the
  // `textInput` event, which carries no type and which no deletion or cut fires.
  const bodyFieldRef = useCallback((node: HTMLTextAreaElement | null) => {
    element.current = node;
    if (node === null) return;
    const recordEdit = (input: InputEvent) => {
      let start = node.selectionStart;
      let end = node.selectionEnd;
      if (start === end && input.inputType === "deleteContentBackward")
        start = Math.max(0, start - 1);
      if (start === end && input.inputType === "deleteContentForward")
        end = Math.min(node.value.length, end + 1);
      pendingTextEdit.current = {
        before: node.value,
        inputType: input.inputType,
        range: { end, start },
      };
    };
    const pickerKeyDown = (event: globalThis.KeyboardEvent) => pickerKey.current(event);
    node.addEventListener("beforeinput", recordEdit);
    node.addEventListener("keydown", pickerKeyDown);
    return () => {
      node.removeEventListener("beforeinput", recordEdit);
      node.removeEventListener("keydown", pickerKeyDown);
      element.current = null;
    };
  }, []);

  /** One suggestion: a button that inserts the mention, at its place in the list the arrows
   *  step through (`navigableOptions`), which is the id the field names as active. Both halves
   *  of the list draw it, so a role nobody is running reads as every other row does. */
  const optionButton = (option: MentionOption, index: number) => {
    const isRole = option.target.startsWith("role:");
    // What the detail line reads, for its `title`: the same relative time the `<Timestamp>` in
    // it draws, and what is left of the line once it is cut. A role nobody is running has no
    // session running to have been seen, so its line ends at the role.
    const lastSeen =
      option.lastSeen === undefined
        ? undefined
        : (relativeTimeLabel(new Date(option.lastSeen), Date.now()) ?? option.lastSeen);
    const line = isRole ? `${option.holder} · ${option.detail}` : option.detail;
    const detail = lastSeen === undefined ? line : `${line} · ${lastSeen}`;
    // Each suggestion is one option that inserts the mention; a listbox owns only options, so
    // the agent in it is drawn by `AgentName` without a link (exclusion E2): a session option's
    // name, and a role option's holder - for a role no live agent holds, its newest session.
    const agent = (
      <AgentName kind="static" sessionId={option.sessionId} storedTitle={option.sessionTitle} />
    );
    return (
      // Below `xl` every button is `inline-flex` with centred items (styles.css's touch rule,
      // unlayered, so it beats `block`): the title and detail stack explicitly and each
      // stretches to the row's width.
      // A session option's full id is its tooltip and accessible description: its name can be
      // the short form `session:<8>…`, which two sessions may share.
      <button
        aria-label={option.title}
        aria-selected={option === activeOption}
        // The active option wears the accent border every selected row in the page wears
        // (`SearchPalette`), never a fill: the detail line's muted text would not clear AA
        // over a sky card.
        className={`flex min-h-11 w-full flex-col rounded-lg border px-3 py-2 text-left text-sm ${surfaceMutedHoverBg} ${option === activeOption ? selectedCardBorder : borderDefault}`}
        id={`${listboxId}-${index}`}
        key={option.target}
        onMouseDown={(event) => event.preventDefault()}
        onClick={() => addMention(option)}
        role="option"
        title={isRole ? undefined : option.sessionId}
        type="button"
      >
        <span className={`self-stretch font-medium ${textPrimaryOnSurface}`}>
          {isRole ? option.title : agent}
        </span>
        <TruncatedText className={`self-stretch text-xs ${textMutedOnSurface}`} title={detail}>
          {isRole ? (
            <>
              {agent} · {option.detail}
            </>
          ) : (
            option.detail
          )}
          {option.lastSeen === undefined ? null : (
            <>
              {" · "}
              <Timestamp at={option.lastSeen} />
            </>
          )}
        </TruncatedText>
      </button>
    );
  };

  return (
    <>
      <label className={`block text-sm font-medium ${textSecondaryOnSurface}`}>
        {labelHidden ? null : label}
        <textarea
          aria-activedescendant={activeId}
          aria-controls={open ? listboxId : undefined}
          aria-label={label}
          className={`mt-1 block w-full rounded-lg border px-3 py-2 font-normal outline-none ${inline ? "min-h-11 resize-none" : "min-h-24"} ${inputClasses(true)} ${textPrimaryOnSurface}`}
          disabled={disabled}
          onChange={(event) => changed(event.target.value, event.target.selectionStart)}
          onDrop={onDrop}
          onInput={
            inline
              ? (event: SyntheticEvent<HTMLTextAreaElement>) => {
                  event.currentTarget.style.height = "auto";
                  event.currentTarget.style.height = `${event.currentTarget.scrollHeight}px`;
                }
              : undefined
          }
          onKeyDown={onKeyDown}
          onPaste={onPaste}
          placeholder={placeholder}
          ref={bodyFieldRef}
          rows={1}
          value={body}
          {...(handle === undefined ? {} : { [handle]: "" })}
        />
      </label>
      {activeMentions.length === 0 ? null : (
        <ul
          aria-label="Mention targets"
          className={`flex flex-wrap gap-2 text-xs ${textPrimaryOnSurface}`}
        >
          {activeMentions.map((mention) => (
            <li className="rounded-full border px-2 py-1" key={mention.target}>
              {mention.text} · <code>{mention.target}</code>
            </li>
          ))}
        </ul>
      )}
      {open ? (
        <div
          aria-label="Mention suggestions"
          className={`max-h-52 overflow-y-auto rounded-lg border p-1 ${surfaceRecessedBg}`}
          id={listboxId}
          role="listbox"
        >
          {filteredOptions.map(optionButton)}
          {/* The issue's Legion roles no live agent holds, after the live options: a mention of
              one is addressed to its role as any role option is, and the arrows step into the
              group from the live options above it. */}
          {filteredStoppedOptions.length === 0 ? null : (
            // biome-ignore lint/a11y/useSemanticElements: a listbox owns only options and groups of options; a fieldset is neither.
            <div aria-labelledby={notRunningId} role="group">
              <div
                className={`px-3 pt-2 pb-1 text-xs font-semibold ${textMutedOnSurface}`}
                id={notRunningId}
                role="presentation"
              >
                Not running
              </div>
              {filteredStoppedOptions.map((option, index) =>
                optionButton(option, filteredOptions.length + index)
              )}
            </div>
          )}
          {/* Only a current list may say nobody matches: one that is refilling, still out or
              unreachable says that instead, so an absent registry never reads as an empty one.
              And only when the picker offers nothing at all: a "Not running" group with options
              in it is a match, so saying no agent matches underneath it would contradict the
              list the reader is looking at. */}
          {listing === "current" ? (
            navigableOptions.length === 0 ? (
              <p className={`px-3 py-2 text-sm ${textMutedOnSurface}`}>No live agents match.</p>
            ) : null
          ) : listing === "refilling" ? (
            <AgentListRefilling className={`px-3 py-2 text-sm ${textMutedOnSurface}`} />
          ) : listing === "loading" ? (
            <p
              className={`px-3 py-2 text-sm ${textMutedOnSurface}`}
              data-testid="agent-list-loading"
            >
              Loading live agents…
            </p>
          ) : (
            <p className={`px-3 py-2 text-sm ${dangerText}`} data-testid="agent-list-unreachable">
              Could not load live agents — Envoy listener unreachable.
            </p>
          )}
        </div>
      ) : null}
    </>
  );
}
