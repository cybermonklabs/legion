import {
  ComposerPrimitive,
  type Unstable_DirectiveFormatter,
  type Unstable_TriggerItem,
  type Unstable_TriggerMatcher,
  unstable_useTriggerPopoverScopeContext,
} from "@assistant-ui/react";
import { type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";

import { TruncatedText } from "../../components/TruncatedText";
import {
  card,
  cardHoverBorder,
  dangerText,
  selectedCardBg,
  selectedCardBorder,
  textMutedOnSurface,
  textOptionDescription,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { RefKindIcon } from "../refs/kind-icons";
import {
  insertionText,
  REF_PICKER_HEADINGS,
  REF_PICKER_KINDS,
  REF_PICKER_TRIGGERS,
  REF_PICKER_WORDS,
  type RefPickerDoor,
  type RefPickerKind,
  readTrigger,
} from "../refs/ref-picker";
import { type RefPicker, useRefPicker } from "../refs/useRefPicker";

/**
 * The three reference doors in an agent's live composer: `[[`, `@` and `#`, drawing the one
 * picker every other box draws (`features/refs/useRefPicker.ts`), beside the `/` that lists the
 * session's own commands. The four are triggers of one input: assistant-ui owns the detection,
 * the highlight and the keyboard, so Enter takes the highlighted row while a list is open and
 * sends the message while none is, and a pick replaces the trigger the reader typed with the
 * row's text.
 *
 * The `/` list stays its own (`AgentThread.tsx`'s `SlashCommands`): a slash command is the
 * characters a terminal reads, not a reference, and it is serialized as the person would type
 * it rather than as an address.
 */

/** The kinds this box offers. A message of a session's live conversation belongs to no issue,
 *  so it has no `dispatch://` address and neither have its blocks: neither kind is offered
 *  here, and the search is never asked for one. */
export const AGENT_MESSAGE_KINDS: readonly RefPickerKind[] = REF_PICKER_KINDS.filter(
  (kind) => kind !== "message" && kind !== "message-block"
);

/** The characters that open each door, as the picker reads them. */
function doorChars(door: RefPickerDoor): string {
  const trigger = REF_PICKER_TRIGGERS.find((candidate) => candidate.door === door);
  if (trigger === undefined) throw new Error(`no trigger opens the ${door} door`);
  return trigger.chars;
}

/**
 * Where this door stands open, by the picker's own rule (`readTrigger`), so the three doors
 * cannot be open at once and a door opens here exactly where it opens in every other box: at a
 * word boundary, with no space typed since. The replaced stretch runs from the trigger's first
 * character to the caret, which is what a pick writes over.
 */
function doorMatcher(door: RefPickerDoor): Unstable_TriggerMatcher {
  return (text, _trigger, cursor) => {
    const found = readTrigger(text.slice(0, cursor));
    if (found === undefined || found.door !== door) return null;
    return { endOffset: cursor, offset: found.start, query: found.query };
  };
}

/** One matcher per door, built once: a fresh one on every render would be a fresh detection on
 *  every render. */
const DOOR_MATCHERS: Record<RefPickerDoor, Unstable_TriggerMatcher> = {
  all: doorMatcher("all"),
  issues: doorMatcher("issues"),
  parties: doorMatcher("parties"),
};

/** One row of the list: its kind's icon, its words, and what it is or where it is under them. */
function RefRow({ index, item }: { index: number; item: Unstable_TriggerItem }): ReactNode {
  const highlighted = unstable_useTriggerPopoverScopeContext().highlightedIndex === index;
  const row = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (highlighted) row.current?.scrollIntoView({ block: "nearest" });
  }, [highlighted]);
  return (
    <ComposerPrimitive.Unstable_TriggerPopoverItem
      className={`flex min-h-11 w-full items-center gap-2 rounded-lg border px-3 py-2 text-left text-sm ${cardHoverBorder} ${
        highlighted ? `${selectedCardBg} ${selectedCardBorder}` : card
      }`}
      index={index}
      item={item}
      // Keeps the caret in the message, so the person types on after picking.
      onMouseDown={(event) => event.preventDefault()}
      ref={row}
    >
      <RefKindIcon kind={item.type as RefPickerKind} />
      <span className="flex min-w-0 flex-1 flex-col">
        <span className={`truncate font-medium ${textPrimaryOnSurface}`}>{item.label}</span>
        {item.description === undefined ? null : (
          <TruncatedText
            className={`self-stretch text-xs ${textOptionDescription}`}
            title={item.description}
          >
            {item.description}
          </TruncatedText>
        )}
      </span>
    </ComposerPrimitive.Unstable_TriggerPopoverItem>
  );
}

/** The rows under their kinds' headings, in the order the picker ranked them for this door. */
function refRows(items: readonly Unstable_TriggerItem[]): ReactNode[] {
  const drawn: ReactNode[] = [];
  let kind: string | undefined;
  items.forEach((item, index) => {
    if (item.type !== kind) {
      kind = item.type;
      drawn.push(
        <div
          className={`px-3 pt-2 pb-1 text-xs font-semibold ${textMutedOnSurface}`}
          key={`heading-${item.type}`}
        >
          {REF_PICKER_HEADINGS[item.type as RefPickerKind]}
        </div>
      );
    }
    drawn.push(<RefRow index={index} item={item} key={item.id} />);
  });
  return drawn;
}

/**
 * The door's own child: it tells the composer above which door is open and what has been typed
 * after it, so the picker reads its rows for that door, and it draws what the list is while it
 * holds no rows. Rendered whether the door is open or not, as every child of a trigger popover
 * is, so everything it draws is behind that check.
 */
function DoorBody({
  door,
  onQuery,
  picker,
}: {
  door: RefPickerDoor;
  onQuery: (door: RefPickerDoor, query: string | undefined) => void;
  picker: RefPicker;
}): ReactNode {
  const { open, query } = unstable_useTriggerPopoverScopeContext();
  useEffect(() => {
    onQuery(door, open ? query : undefined);
  }, [door, onQuery, open, query]);
  // The door closing takes its rows with it.
  useEffect(() => () => onQuery(door, undefined), [door, onQuery]);
  if (!open) return null;
  return (
    <>
      {picker.instruction === undefined ? null : (
        <p
          className={`px-3 py-2 text-xs ${textMutedOnSurface}`}
          data-testid="ref-picker-instruction"
        >
          {picker.instruction}
        </p>
      )}
      {picker.state === "rows" ? null : picker.state === "unreachable" ? (
        <p className={`px-3 py-2 text-sm ${dangerText}`} data-testid="ref-picker-unreachable">
          {REF_PICKER_WORDS.unreachable}
        </p>
      ) : (
        <p
          className={`px-3 py-2 text-sm ${textMutedOnSurface}`}
          data-testid={`ref-picker-${picker.state}`}
        >
          {REF_PICKER_WORDS[picker.state]}
        </p>
      )}
    </>
  );
}

/**
 * The three reference triggers of the live composer. Mounted inside the composer's
 * `Unstable_TriggerPopoverRoot`, beside the `/` trigger, so one input carries four.
 */
export function ReferenceTriggers(): ReactNode {
  /** The door the input has open and what has been typed after it, as the open door's own
   *  child reports it. */
  const [opened, setOpened] = useState<{ door: RefPickerDoor; query: string } | undefined>(
    undefined
  );
  const onQuery = useCallback((door: RefPickerDoor, query: string | undefined) => {
    setOpened((current) => {
      if (query === undefined) return current?.door === door ? undefined : current;
      if (current?.door === door && current.query === query) return current;
      return { door, query };
    });
  }, []);
  // What the picker reads its own trigger from: the door's characters and the query typed after
  // them, which is the draft as far as a reference is concerned. The composer holds the rest of
  // the message, and a pick replaces the trigger alone, which assistant-ui does by the offsets
  // the matcher gave it.
  const text = opened === undefined ? "" : `${doorChars(opened.door)}${opened.query}`;
  const picker = useRefPicker({
    caret: text.length,
    enabled: opened !== undefined,
    kinds: AGENT_MESSAGE_KINDS,
    text,
  });

  const items = useMemo<Unstable_TriggerItem[]>(
    () =>
      picker.groups.flatMap((group) =>
        group.rows.map((row) => ({
          id: row.id,
          label: row.label,
          type: row.kind,
          ...(row.detail === "" ? {} : { description: row.detail }),
        }))
      ),
    [picker.groups]
  );
  // What each row writes, by the one rule every box writes a pick by: a party as a link naming
  // it, every other kind as its bare address.
  const written = useMemo(
    () =>
      new Map(
        picker.groups.flatMap((group) => group.rows.map((row) => [row.id, insertionText(row)]))
      ),
    [picker.groups]
  );
  const adapter = useMemo(
    () => ({ categories: () => [], categoryItems: () => [], search: () => items }),
    [items]
  );
  const formatter = useMemo<Unstable_DirectiveFormatter>(
    () => ({
      parse: (value: string) => [{ kind: "text" as const, text: value }],
      serialize: (item: Unstable_TriggerItem) => written.get(item.id) ?? item.label,
    }),
    [written]
  );

  return (
    <>
      {REF_PICKER_TRIGGERS.map(({ chars, door }) => (
        <ComposerPrimitive.Unstable_TriggerPopover
          adapter={adapter}
          aria-label="References"
          char={chars}
          className={`absolute right-0 bottom-full left-0 z-10 mb-2 flex max-h-72 flex-col gap-1 overflow-y-auto rounded-lg border p-1 shadow-lg ${card}`}
          isLoading={picker.state === "loading"}
          key={chars}
          matcher={DOOR_MATCHERS[door]}
        >
          <ComposerPrimitive.Unstable_TriggerPopover.Directive formatter={formatter} />
          <ComposerPrimitive.Unstable_TriggerPopoverItems className="contents">
            {refRows}
          </ComposerPrimitive.Unstable_TriggerPopoverItems>
          <DoorBody door={door} onQuery={onQuery} picker={picker} />
        </ComposerPrimitive.Unstable_TriggerPopover>
      ))}
    </>
  );
}
