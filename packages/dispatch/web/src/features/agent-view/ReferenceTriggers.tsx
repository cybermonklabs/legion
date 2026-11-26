import {
  ComposerPrimitive,
  type Unstable_DirectiveFormatter,
  type Unstable_TriggerItem,
  type Unstable_TriggerMatcher,
  unstable_useTriggerPopoverScopeContext,
} from "@assistant-ui/react";
import {
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import { TruncatedText } from "../../components/TruncatedText";
import {
  cardHoverBorder,
  selectedCardBg,
  selectedCardBorder,
  textOptionDescription,
} from "../../theme/classes";
import { card, textMutedOnSurface } from "../../theme/shell-classes";
import { RefKindIcon } from "../refs/kind-icons";
import { RefPartsControl, RefPickerNotes, RefRowDetail, RefRowLabel } from "../refs/RefPickerList";
import {
  insertionText,
  isCompositionKey,
  orderedRows,
  REF_PICKER_HEADINGS,
  REF_PICKER_TRIGGERS,
  type RefPickerDoor,
  type RefPickerKind,
  type RefPickerRow,
  readTrigger,
} from "../refs/ref-picker";
import { type RefPicker, useRefPicker } from "../refs/useRefPicker";

/**
 * The three reference doors in an agent's live composer: `[[`, `@` and `#`, drawing the one
 * picker every other box draws (`features/refs/useRefPicker.ts`) over every kind it reaches,
 * beside the `/` that lists the session's own commands. The four are triggers of one input:
 * assistant-ui owns the detection, the highlight and the keyboard, so Enter takes the highlighted
 * row while a list is open and sends the message while none is, and a pick replaces the trigger
 * the reader typed with the row's text. A row that holds parts - a node's blocks, an agent's
 * documents - carries the control every other box draws for them (`RefPartsControl`);
 * ArrowRight opens the highlighted row's and ArrowLeft closes them again, two keys assistant-ui
 * leaves to the input: the composer hands them here first (`ReferenceKeys`).
 *
 * The `/` list stays its own (`AgentThread.tsx`'s `SlashCommands`): a slash command is the
 * characters a terminal reads, not a reference, and it is serialized as the person would type
 * it rather than as an address.
 */

/** What the composer's input hands the reference doors before assistant-ui reads the key: the
 *  doors stop the one key they serve themselves, and leave every other to the composer. */
export type ReferenceKeys = (event: KeyboardEvent<HTMLTextAreaElement>) => void;

/** The characters that open each door, as the picker reads them. */
function doorChars(door: RefPickerDoor): string {
  const trigger = REF_PICKER_TRIGGERS.find((candidate) => candidate.door === door);
  if (trigger === undefined) throw new Error(`no trigger opens the ${door} door`);
  return trigger.chars;
}

/** Whether each door's trigger stands in a written link's address, as that door's matcher last
 *  read it (`RefPickerTrigger.inAddress`). */
type DoorsInAddress = Record<RefPickerDoor, boolean>;

/**
 * Where this door stands open, by the picker's own rule (`readTrigger`), so the three doors
 * cannot be open at once and a door opens here exactly where it opens in every other box: at a
 * word boundary, with no space typed since, never in a written link's words, and in its address
 * only on a trigger typed there, with the caret at the address's end. The rule reads the text on
 * both sides of the caret, so the whole message goes to it. The replaced stretch runs from the
 * trigger's first character to the caret, which is what a pick writes over. The matcher is the
 * one reader that sees the whole message and the caret, and assistant-ui hands the formatter the
 * picked item alone, so the matcher leaves in `inAddress` whether its door stands in a link's
 * address, which decides what a pick there writes (`insertionText`).
 */
function doorMatcher(
  door: RefPickerDoor,
  inAddress: RefObject<DoorsInAddress>
): Unstable_TriggerMatcher {
  return (text, _trigger, cursor) => {
    const found = readTrigger(text, cursor);
    if (found === undefined || found.door !== door) return null;
    inAddress.current[door] = found.inAddress;
    return { endOffset: cursor, offset: found.start, query: found.query };
  };
}

/** One row of the list: its kind's icon, its words, what it is or where it is under them, and
 *  the control that opens its parts where it holds any. */
function RefRow({
  index,
  item,
  picker,
  row,
}: {
  index: number;
  item: Unstable_TriggerItem;
  picker: RefPicker;
  /** The picker's row this item draws: its kind, and whether it holds parts. */
  row: RefPickerRow;
}): ReactNode {
  const highlighted = unstable_useTriggerPopoverScopeContext().highlightedIndex === index;
  const element = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (highlighted) element.current?.scrollIntoView({ block: "nearest" });
  }, [highlighted]);
  return (
    // The item draws as a `div` rather than assistant-ui's own button, for the reason
    // `RefPickerList`'s row is one: a row that holds parts carries the control that lists them,
    // and a button inside a button is no button at all. The item's own props - its option role,
    // its highlight and the click that takes it - land on the `div`. A session's row has the
    // session's full id as its title, as `RefPickerList`'s does: its name can be the short form
    // `session:<8>…`, which two sessions may share.
    <ComposerPrimitive.Unstable_TriggerPopoverItem asChild index={index} item={item}>
      <div
        aria-selected={highlighted}
        className={`flex min-h-11 w-full items-center gap-2 rounded-lg border px-3 py-2 text-left text-sm ${cardHoverBorder} ${
          highlighted ? `${selectedCardBg} ${selectedCardBorder}` : card
        }`}
        // Keeps the caret in the message, so the person types on after picking a row or opening
        // its parts.
        onMouseDown={(event) => event.preventDefault()}
        ref={element}
        role="option"
        tabIndex={-1}
        title={row.kind === "agent" ? row.agent?.sessionId : undefined}
      >
        <RefKindIcon kind={row.kind} />
        <span className="flex min-w-0 flex-1 flex-col">
          <RefRowLabel row={row} />
          {item.description === undefined ? null : (
            <TruncatedText
              className={`self-stretch text-xs ${textOptionDescription}`}
              title={item.description}
            >
              <RefRowDetail row={row} />
            </TruncatedText>
          )}
        </span>
        <RefPartsControl picker={picker} row={row} />
      </div>
    </ComposerPrimitive.Unstable_TriggerPopoverItem>
  );
}

/** The rows under their kinds' headings, in the order the picker ranked them for this door. */
function refRows(
  items: readonly Unstable_TriggerItem[],
  rows: ReadonlyMap<string, RefPickerRow>,
  picker: RefPicker
): ReactNode[] {
  const drawn: ReactNode[] = [];
  let kind: RefPickerKind | undefined;
  items.forEach((item, index) => {
    // Every item is built from one of these rows, so each has one.
    const row = rows.get(item.id);
    if (row === undefined) return;
    if (row.kind !== kind) {
      kind = row.kind;
      drawn.push(
        <div
          className={`px-3 pt-2 pb-1 text-xs font-semibold ${textMutedOnSurface}`}
          key={`heading-${row.kind}`}
        >
          {REF_PICKER_HEADINGS[row.kind]}
        </div>
      );
    }
    drawn.push(<RefRow index={index} item={item} key={item.id} picker={picker} row={row} />);
  });
  return drawn;
}

/**
 * The door's own child: it tells the composer above which door is open, what has been typed
 * after it and which row assistant-ui has highlighted, so the picker reads its rows for that door
 * and ArrowRight knows whose parts to open, and it draws what the list is while it holds no rows.
 * Rendered whether the door is open or not, as every child of a trigger popover is, so everything
 * it draws is behind that check.
 */
function DoorBody({
  door,
  highlighted,
  onQuery,
  picker,
}: {
  door: RefPickerDoor;
  /** The open door's highlighted row, which only that door's own scope can read. */
  highlighted: RefObject<string | undefined>;
  onQuery: (door: RefPickerDoor, query: string | undefined) => void;
  picker: RefPicker;
}): ReactNode {
  const { highlightedIndex, items, open, query } = unstable_useTriggerPopoverScopeContext();
  useEffect(() => {
    onQuery(door, open ? query : undefined);
  }, [door, onQuery, open, query]);
  // The door closing takes its rows with it.
  useEffect(() => () => onQuery(door, undefined), [door, onQuery]);
  useEffect(() => {
    if (!open) return;
    highlighted.current = items[highlightedIndex]?.id;
    return () => {
      highlighted.current = undefined;
    };
  }, [highlighted, highlightedIndex, items, open]);
  if (!open) return null;
  return <RefPickerNotes picker={picker} />;
}

/**
 * The three reference triggers of the live composer. Mounted inside the composer's
 * `Unstable_TriggerPopoverRoot`, beside the `/` trigger, so one input carries four. `keys` is
 * what that input hands every key to before assistant-ui reads it, which the triggers fill.
 */
export function ReferenceTriggers({ keys }: { keys: RefObject<ReferenceKeys | null> }): ReactNode {
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
    text,
  });

  // Each row by its id: assistant-ui's items carry an item's words alone, and what a row writes
  // and whether it holds parts are the picker's row's.
  const rows = useMemo(
    () => new Map(orderedRows(picker.groups).map((row) => [row.id, row])),
    [picker.groups]
  );
  const items = useMemo<Unstable_TriggerItem[]>(
    () =>
      [...rows.values()].map((row) => ({
        id: row.id,
        label: row.label,
        type: row.kind,
        ...(row.detail === "" ? {} : { description: row.detail }),
      })),
    [rows]
  );
  const adapter = useMemo(
    () => ({ categories: () => [], categoryItems: () => [], search: () => items }),
    [items]
  );
  // One matcher per door, built once: a fresh one on every render would be a fresh detection on
  // every render. Each leaves here whether its door stands in a link's address.
  const inAddress = useRef<DoorsInAddress>({ all: false, issues: false, parties: false });
  const matchers = useMemo<Record<RefPickerDoor, Unstable_TriggerMatcher>>(
    () => ({
      all: doorMatcher("all", inAddress),
      issues: doorMatcher("issues", inAddress),
      parties: doorMatcher("parties", inAddress),
    }),
    []
  );
  // What each row writes, by the one rule every box writes a pick by: a party as a link naming
  // it, every other kind as its bare address, and any kind as its bare address where its door
  // stands in a link's address.
  const formatters = useMemo<Record<RefPickerDoor, Unstable_DirectiveFormatter>>(() => {
    const formatter = (door: RefPickerDoor): Unstable_DirectiveFormatter => ({
      parse: (value: string) => [{ kind: "text" as const, text: value }],
      serialize: (item: Unstable_TriggerItem) => {
        const row = rows.get(item.id);
        return row === undefined
          ? item.label
          : insertionText(row, { inAddress: inAddress.current[door] });
      },
    });
    return { all: formatter("all"), issues: formatter("issues"), parties: formatter("parties") };
  }, [rows]);

  // ArrowRight opens the highlighted row's parts and ArrowLeft closes them, as they do in every
  // other box (`useRefPicker`'s `pressKey`). assistant-ui reads neither key, so the input would
  // move its caret instead; the composer hands them here first, and each is stopped only where
  // there are parts to open or to close. The handler reads the newest picker and rows when the
  // key comes.
  const highlighted = useRef<string | undefined>(undefined);
  const pickerNow = useRef(picker);
  pickerNow.current = picker;
  const rowsNow = useRef(rows);
  rowsNow.current = rows;
  useEffect(() => {
    keys.current = (event) => {
      // A composition's keys are the input method's: its arrows move between the word's segments.
      if (isCompositionKey(event.nativeEvent)) {
        // assistant-ui skips a key marked `isComposing` itself, but not Safari's confirming
        // Enter, which carries only the IME key code 229. Its default is prevented, so the
        // library's own key handler never runs: Radix's `composeEventHandlers` skips it after a
        // host `onKeyDown` that prevented the default. No other composition key is prevented:
        // Chrome's first key of a composition (`Process`) carries the same code, and preventing
        // it stops the input method. Safari's confirming Escape is out of this handler's reach:
        // the library reads Escape on a capture listener of the document, which runs first.
        const { isComposing, keyCode } = event.nativeEvent;
        if (event.key === "Enter" && !isComposing && keyCode === 229) event.preventDefault();
        return;
      }
      if (event.key === "ArrowLeft") {
        if (pickerNow.current.pressKey("ArrowLeft").action !== "ignored") event.preventDefault();
        return;
      }
      const id = highlighted.current;
      if (event.key !== "ArrowRight" || id === undefined) return;
      if (rowsNow.current.get(id)?.parts === undefined) return;
      event.preventDefault();
      pickerNow.current.openParts(id);
    };
    return () => {
      keys.current = null;
    };
  }, [keys]);

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
          matcher={matchers[door]}
        >
          <ComposerPrimitive.Unstable_TriggerPopover.Directive formatter={formatters[door]} />
          <ComposerPrimitive.Unstable_TriggerPopoverItems className="contents">
            {(shown) => refRows(shown, rows, picker)}
          </ComposerPrimitive.Unstable_TriggerPopoverItems>
          <DoorBody door={door} highlighted={highlighted} onQuery={onQuery} picker={picker} />
        </ComposerPrimitive.Unstable_TriggerPopover>
      ))}
    </>
  );
}
