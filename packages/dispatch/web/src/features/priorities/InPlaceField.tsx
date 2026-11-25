import {
  type ChangeEvent,
  type KeyboardEvent,
  type ReactNode,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";
import { inputMethodKey } from "../../lib/input-method-key";
import {
  bgTransparent,
  borderStrongHover,
  borderTransparent,
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  focusBorder,
  focusVisibleRing,
  inputClasses,
  surfaceMutedStrongBg,
  textPrimaryOnSurface,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { answered, arrival, NOTHING_KEPT, type SentText, usedOn } from "./field-arrival";

/** How long the reader may stop typing before what they wrote is saved. Nothing else in
 *  Dispatch asks a person to press Save, so a priority's fields are saved as they are written:
 *  once the typing rests, at once when the field is left, and as the page goes, so far as the
 *  page lets a write start and finish then (`api/client.ts`'s `pageGoing`). The description's
 *  editor keeps the same rule (`PriorityDescription`). */
export const SAVE_AFTER_MS = 500;

/**
 * What a field of a priority shows beside itself while it holds another person's text and the
 * reader's own is not written: the reader's texts, newest first, since a second change can arrive
 * before they choose, and their choices. Using one puts it back and writes it at once, over the
 * other person's text, and lets the others go; keeping theirs lets every one go. A title's or a
 * description's write names the text it goes over (`usePriorityWrites`), so it is refused if a
 * third text arrived meanwhile; the owner's write names none. Nothing is dropped until the reader
 * picks. It is an alert, since the reader's text has just left the field they were writing in.
 * A one-line field names it as what describes it (`aria-describedby`, by `id`), so the two stay
 * tied wherever it is drawn (`InPlaceField`'s `noticeAt`). It is drawn in spans and buttons
 * alone, so it may stand wherever a field may, a span included.
 */
export function ChangedMeanwhile({
  id,
  label,
  mine,
  onKeepTheirs,
  onUse,
}: {
  /** The notice's own id, which the one-line field it stands for names in `aria-describedby`. */
  id?: string;
  /** The field's own name, which names the notice. */
  label: string;
  /** The reader's texts, newest first, each drawn as written: every line of it, its spaces kept. */
  mine: readonly string[];
  onKeepTheirs: () => void;
  /** Puts `text`, one of `mine`, back and writes it. */
  onUse: (text: string) => void;
}): ReactNode {
  return (
    <span
      aria-label={`${label}, changed by another person`}
      className={`mt-1 block space-y-1 rounded-lg border p-2 text-sm font-normal ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
      id={id}
      role="alert"
    >
      {mine.length === 1 ? (
        <>
          <span className="block">Another person changed this while you wrote. Your text:</span>
          <span className="block whitespace-pre-wrap border-l-2 pl-2">{mine[0]}</span>
          <span className="flex flex-wrap gap-3">
            <button
              className="font-semibold underline"
              onClick={() => onUse(mine[0])}
              type="button"
            >
              Use my text
            </button>
            <button className="underline" onClick={onKeepTheirs} type="button">
              Keep their text
            </button>
          </span>
        </>
      ) : (
        <>
          <span className="block">
            Another person changed this while you wrote. Your texts, newest first:
          </span>
          {mine.map((text) => (
            <span className="block space-y-1" key={text}>
              <span className="block whitespace-pre-wrap border-l-2 pl-2">{text}</span>
              <button className="font-semibold underline" onClick={() => onUse(text)} type="button">
                Use this text
              </button>
            </span>
          ))}
          <button className="block underline" onClick={onKeepTheirs} type="button">
            Keep their text
          </button>
        </>
      )}
    </span>
  );
}

/** Stops the write waiting for the typing to rest, if one waits. */
function stopTimer(timer: { current: number | undefined }): void {
  window.clearTimeout(timer.current);
  timer.current = undefined;
}

/** What a field holds, as it is compared and written: trimmed, and in a `required` field the
 *  reader emptied, the text it compares with (`base`), since an empty one is never written. */
function heldText(draft: string, base: string, required: boolean): string {
  const typed = draft.trim();
  return required && typed === "" ? base : typed;
}

/** The type an issue's title is drawn in, in its header (`IssueHeader.tsx`): a field drawn as a
 *  heading reads in it, and so does the box it opens into. */
const TITLE_TYPE = "text-[22px] leading-7 font-semibold tracking-tight";

/** The box of a field drawn as a heading (`closed="heading"`), and of the heading a reader who
 *  may not write it reads: the width of its line, and its words set in by the padding the open
 *  box writes in, which the negative margin gives back, so the words start where the rows under
 *  the heading do and stay put as it opens. */
export const HEADING_BOX = `-mx-2 min-w-0 break-words rounded-lg border px-2 py-1 md:py-0 ${TITLE_TYPE} ${borderTransparent} ${textPrimaryOnSurface}`;

/** A label chip's shape and tone (`IssueLabels.tsx`), which a field drawn as a chip
 *  (`closed="chip"`) and the chip a reader who may not write it reads both take. Its words wrap
 *  inside it rather than run past the row it stands in. */
export const FIELD_CHIP = `inline-flex min-h-11 max-w-full items-center gap-1 rounded-full px-3 py-1 text-left text-xs font-medium [overflow-wrap:anywhere] sm:min-h-7 sm:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface}`;

/**
 * A one-line field edited where it is read: no edit mode, no Save button. What the reader types
 * is kept here until it rests, then written once; leaving the field writes it immediately, and
 * Enter does both, as does the field leaving the page. The page going (`pagehide`) makes the
 * write at once too, and `api/client.ts`'s `pageGoing` says when it starts and is let finish
 * then. Only a text that differs from the last one the field wrote or took in is written, so
 * leaving a field the reader typed nothing into writes nothing. The texts compared are trimmed,
 * since every field drawn here is stored trimmed (`internal/dispatch/api/priorities.go`), and
 * what is written is trimmed. A `required` field never writes an empty text: left empty, it shows
 * that last text again.
 *
 * A change of `value` is the server's text moving under the field, which it reads as the
 * description's editor does (`arrival`). The round trip of one of its own writes changes nothing
 * the reader sees, so their text never seems undone while it travels. Another person's text is
 * taken in at once - with the focus in the field or not - while the reader holds no unwritten
 * typing and no write of theirs is out. Else it is taken in all the same, the waiting write is
 * stopped, and the reader's text is kept beside the field (`ChangedMeanwhile`) until they use it
 * again or let it go; where the page gives that notice a place of its own (`noticeAt`), it is
 * drawn there, and the field names it as what describes it either way. Where the page holds
 * those texts (`kept`, `onKeptChange`) - a priority's title, whose texts the write store holds
 * (`usePriorityWrites`) - they outlive the field, the page and a reload, and a field drawn again
 * shows them again. Else the field holds them itself, and they go with it: the owner's field.
 * Each write hands on, with the text written, the server's text the field last took in (`onSave`'s
 * `from`): the text it was drawn with, or the last that arrived. A title's write names it as the
 * text it goes over (`expected`), unless a write of the page's own goes before it. Only a read
 * moves it, never a write, which may not land, and never the page's own read of the text: the page
 * draws another person's text before the field takes it in, and a write made in between - the
 * typing at rest, the field left, Enter, the page going - names the text the reader edited, so
 * Dispatch refuses it, and the reader's text is kept beside the other person's as it is where that
 * text arrives first. Use my text writes the kept text at once, whatever the field held before,
 * unless the field's newest write is that very text and still out. The page answers each write
 * with whether it landed: one that did is no write of the reader's still out when another person's
 * text arrives, and one that did not is no write whose answer the field waits for (`sent`), and no
 * text the field writes again on its own once the write before it comes back: the reader discarded
 * it, or the shell's line holds it, with its Retry. Each write hands on too the text the field
 * held as its edit began (`onSave`'s `on`, `made`): Use my text's as well, since it puts the
 * kept text in place of what the field holds - but for Use my text of a text a write of the
 * reader's carried, which is that same edit, and hands on what that write was made on: the
 * field's own record of it, or, in a field drawn again, the page's (`keptMadeOn`). That is not
 * always the text the field compares with (`base`): a read that answers an earlier write while
 * the reader's typing waits moves `base` to that write, but the typing began on what the field
 * held. A field drawn again holds the server's text, so its first edit is made on that, whatever
 * a field before it wrote.
 *
 * Drawn `closed`, the field reads as its text until the reader opens it, and closes as they leave
 * it; what it writes, and when, is the same. A heading (`closed="heading"`) is the page's heading,
 * in an issue title's type, as an issue's header draws its title: a click, the focus, Enter or
 * Space opens it, and open, the heading holds a box in the same type that wraps as the heading
 * does, so every word of a long title stays in view as it is written, and nothing under it moves
 * as it opens and closes. A chip (`closed="chip"`) reads the field's name and its text in a label
 * chip's shape (`FIELD_CHIP`), and a click opens it into the plain box, its name beside it. While
 * it is closed, the heading or the chip is what names the notice as what describes it.
 */
export function InPlaceField({
  closed,
  kept: keptByPage,
  keptMadeOn,
  label,
  maxLength,
  noticeAt,
  onKeptChange,
  onSave,
  options,
  placeholder,
  required = false,
  value,
}: {
  /** What the field is while the reader is not writing in it: the page's heading, or a chip.
   *  Absent, it is always open: the plain bordered box. */
  closed?: "chip" | "heading";
  /** The reader's texts kept beside the field, where the page holds them rather than the field -
   *  a priority's title, in the write store (`usePriorityWrites`): `onKeptChange` hears each
   *  change of them. */
  kept?: readonly string[];
  /** What the write of `text`, one of the kept texts the page holds, was made on, where the page
   *  holds that write and it recorded that: a field drawn again wrote none of them itself. */
  keptMadeOn?: (text: string) => string | undefined;
  label: string;
  /** The most characters the field holds, where Dispatch bounds the text, as it bounds a title
   *  (`ISSUE_TITLE_MAX`): a longer text is never typed into the field, so never sent. */
  maxLength?: number;
  /** Where the notice of the reader's kept texts is drawn, where the page gives it a place of its
   *  own: a field standing in a row of chips would else draw it inside the row. `null` is a place
   *  not drawn yet, which holds no notice until it is; absent, the notice is drawn beside the
   *  field, after a heading's heading. */
  noticeAt?: HTMLElement | null;
  onKeptChange?: (kept: readonly string[]) => void;
  /** Writes `next`, the reader's text, over `from`, the server's text the field last took in,
   *  which a title's write names as the text it goes over (`expected`); the owner's names none.
   *  `on` is the text the edit was made on: the text the field held as the edit began - the last
   *  it wrote, though that write did not land, or the last it took in, whichever came last - or,
   *  for Use my text of a text whose own write is known, what that write was made on, since it
   *  writes that same edit again. So a write made on a stopped one's text is known to carry it
   *  (`PriorityTextWrite.made`). Answers, where the page tells, whether the write landed. */
  // biome-ignore lint/suspicious/noConfusingVoidType: a page that does not say whether its write landed - the owner's - hands any callback, as a void callback takes one
  onSave: (next: string, from: string, on: string) => Promise<boolean> | void;
  /** What other rows already hold, offered beside the field: a reader picks one of the owners
   *  the deployment uses, or writes a new one, since nothing bounds what an owner may be. */
  options?: readonly string[];
  /** What the open box shows while it is empty, and a chip reads while its text is. */
  placeholder?: string;
  /** Whether the text may not be empty, as a title may not: an empty one is never written. */
  required?: boolean;
  value: string;
}): ReactNode {
  const [draft, setDraft] = useState(value);
  // The reader's own texts, kept beside the field while it shows another person's, where the
  // page does not hold them.
  const [ownKept, setOwnKept] = useState(NOTHING_KEPT);
  const kept = keptByPage ?? ownKept;
  const setKept = onKeptChange ?? setOwnKept;
  const timer = useRef<number | undefined>(undefined);
  const noticeId = useId();
  const fieldId = useId();
  // Whether the reader has opened a field drawn `closed`; any other is always open.
  const [opened, setOpened] = useState(false);
  const box = useRef<HTMLInputElement | HTMLTextAreaElement | null>(null);
  // Where the field stands against the server's text (`FieldStand`): what the reader's text is
  // compared with, and the texts written that the server has not answered with yet, oldest first.
  const base = useRef(value);
  const sent = useRef<readonly SentText[]>([]);
  // The text the reader's edit is made on (`onSave`'s `on`): the last text the field wrote, or
  // the last of the server's it came to hold - another person's it took in, or one a read
  // brought while the reader held it (`arrival`). A read that moves `base` under typing she has
  // not written leaves it: that typing began on it.
  const made = useRef(value);
  // What each text the field wrote was made on, by the text: Use my text of one of them writes
  // that same edit again, and is made on what it was made on (`putBack`).
  const [madeOn] = useState(() => new Map<string, string>());
  // The server's text the field last took in: the text it was drawn with, and then each that
  // arrived (`arrival`). A read is all that moves it; each write hands it on (`onSave`'s `from`).
  const seen = useRef(value);
  const latest = useRef({ draft, kept, onSave, required, setKept });
  latest.current = { draft, kept, onSave, required, setKept };

  // Writes `text`, made on `on`: `made`, unless Use my text says otherwise. The page's answer is
  // that write's own (`answered`): one that landed is no write still out, and one that did not
  // never comes back, so the field waits for no answer with it.
  const write = (text: string, on = made.current) => {
    base.current = text;
    made.current = text;
    madeOn.set(text, on);
    const entry = { landed: false, text };
    sent.current = [...sent.current, entry];
    void latest.current.onSave(text, seen.current, on)?.then((landed) => {
      sent.current = answered(sent.current, entry, landed);
    });
  };
  const save = () => {
    stopTimer(timer);
    const text = heldText(latest.current.draft, base.current, latest.current.required);
    if (text !== base.current) write(text);
  };
  const saveNow = useRef(save);
  saveNow.current = save;

  useEffect(() => {
    if (value === seen.current) return;
    seen.current = value;
    const { draft: current, kept: mine, required: needed, setKept: keep } = latest.current;
    const next = arrival(value, heldText(current, base.current, needed), {
      base: base.current,
      kept: mine,
      made: made.current,
      sent: sent.current,
    });
    base.current = next.base;
    made.current = next.made;
    sent.current = next.sent;
    if (next.kept !== mine) keep(next.kept);
    if (!next.take) return;
    stopTimer(timer);
    setDraft(value);
  }, [value]);

  // Leaving the page, or the field leaving it, leaves the field: what the reader typed is
  // written now rather than lost with the timer. A small write that starts as the page goes is
  // let finish as it goes (`keepalive`, `api/client.ts`'s `pageGoing`); one that waits behind a
  // write of the same priority still out, or behind a kept write of the same priority a waiting
  // take-up will send (`Writes.run`), does not start before the page is gone. The tab keeps it
  // too, for a reload to make (`features/writes/write-storage.ts`).
  useEffect(() => {
    const leave = () => saveNow.current();
    window.addEventListener("pagehide", leave);
    return () => {
      window.removeEventListener("pagehide", leave);
      leave();
    };
  }, []);

  // Opened, the box takes the focus, the caret after the text, where the reader goes on writing.
  useEffect(() => {
    const node = box.current;
    if (!opened || node === null) return;
    node.focus();
    node.setSelectionRange(node.value.length, node.value.length);
  }, [opened]);

  // A heading's box is as tall as the text it holds, so the title wraps in it as it does in the
  // heading, and what stands under it keeps its place as the field opens, closes or takes a line.
  // biome-ignore lint/correctness/useExhaustiveDependencies: `draft` and `opened` are the triggers; the box is read as it stands
  useLayoutEffect(() => {
    const node = box.current;
    if (closed !== "heading" || node === null) return;
    node.style.height = "auto";
    node.style.height = `${node.scrollHeight + node.offsetHeight - node.clientHeight}px`;
  }, [closed, draft, opened]);

  // One of the reader's kept texts, put back in the field and written at once, even where the
  // field holds it already: the page offered it beside the other person's text, which it goes over.
  // Not where the field's newest write is that very text and still out: it is on its way, and a
  // second write of it would leave an entry in `sent` that no answer clears where the page answers
  // none (the owner's), since a read of that text answers the first of them alone (`arrival`).
  // A text whose own write is known - the field's, or one the page holds - is that same edit
  // written again, made on what that write was made on, and through each kept text the click lets
  // go (`usedOn`); any other is a new edit, made on `made`.
  const putBack = (text: string) => {
    stopTimer(timer);
    setKept(NOTHING_KEPT);
    setDraft(text);
    const newest = sent.current.at(-1);
    if (newest?.text === text && !newest.landed) return;
    const on = usedOn(text, kept, (one) => madeOn.get(one) ?? keptMadeOn?.(one)) ?? made.current;
    write(text, on);
  };
  // The notice of the reader's kept texts: in the place the page gives it once that place is
  // drawn, else beside the field, and named by the field as what describes it wherever it is.
  const notice =
    kept.length === 0 || noticeAt === null ? null : (
      <ChangedMeanwhile
        id={noticeId}
        label={label}
        mine={kept}
        onKeepTheirs={() => setKept(NOTHING_KEPT)}
        onUse={putBack}
      />
    );
  const describedBy = notice === null ? undefined : noticeId;
  const placed = noticeAt ? createPortal(notice, noticeAt) : notice;
  // What the open box does, in every shape: it is the one place the reader writes the text.
  const typing = {
    "aria-describedby": describedBy,
    "aria-label": label,
    maxLength,
    onBlur: () => {
      save();
      setDraft((typed) => heldText(typed, base.current, required));
      setOpened(false);
    },
    onChange: (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      // A heading's box is a textarea, so its title wraps as the heading does; it holds one line
      // all the same, and a line break pasted into it goes, as a one-line field drops it.
      setDraft(event.target.value.replace(/[\r\n]/g, ""));
      stopTimer(timer);
      timer.current = window.setTimeout(save, SAVE_AFTER_MS);
    },
    onKeyDown: (event: KeyboardEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      // The Enter that ends an input method's composition takes the word composed, and is no
      // Enter of the reader's: it neither writes nor leaves the field. Safari sends it after its
      // compositionend, with `isComposing` false and keyCode 229 (`inputMethodKey`).
      if (event.key !== "Enter" || inputMethodKey(event.nativeEvent)) return;
      event.preventDefault();
      save();
      event.currentTarget.blur();
    },
    placeholder,
    ref: (node: HTMLInputElement | HTMLTextAreaElement | null) => {
      box.current = node;
    },
    value: draft,
  };

  if (closed === "heading") {
    return (
      <>
        {opened ? (
          // A title holds no markup, so the box takes no picker: `priority-title` in
          // `DISPATCH_WRITING_BOXES` says why.
          <h1 className={`-mx-2 min-w-0 ${TITLE_TYPE} ${textPrimaryOnSurface}`}>
            <textarea
              {...typing}
              className={`block w-full resize-none overflow-hidden rounded-lg border px-2 py-1 outline-none md:py-0 ${TITLE_TYPE} ${borderTransparent} ${bgTransparent} ${textPrimaryOnSurface} ${borderStrongHover} ${focusBorder}`}
              rows={1}
            />
          </h1>
        ) : (
          <h1
            aria-describedby={describedBy}
            className={`cursor-text ${HEADING_BOX} ${borderStrongHover}`}
            onClick={() => setOpened(true)}
            onFocus={() => setOpened(true)}
            onKeyDown={(event) => {
              if (event.key !== "Enter" && event.key !== " ") return;
              event.preventDefault();
              setOpened(true);
            }}
            // biome-ignore lint/a11y/noNoninteractiveTabindex: the heading is the title's edit control, as the issue title's heading is, and a keyboard reaches it by Tab.
            tabIndex={0}
          >
            {draft}
          </h1>
        )}
        {placed}
      </>
    );
  }

  const listId = options === undefined ? undefined : `${label.replace(/\W+/g, "-")}-options`;
  // A plain bordered field, in the border, the focus border and the focus ring of Dispatch's
  // inputs: open, it is the text's one reading, written where it is read, so nothing hides it
  // until the reader hovers it. Opened from a chip it is as tall as the chip it stands for, 44 px
  // on a phone and 28 px from `sm`, so the row keeps its height.
  const field = (
    <input
      {...typing}
      className={`${closed === undefined ? "w-full py-1" : "min-h-11 w-48 max-w-full py-0.5 sm:min-h-7"} rounded-md border px-2 text-sm outline-none focus-visible:ring-2 ${inputClasses(true)} ${focusVisibleRing} ${textPrimaryOnSurface}`}
      id={fieldId}
      list={listId}
      type="text"
    />
  );
  return (
    <>
      {closed === undefined ? (
        field
      ) : opened ? (
        <span className="inline-flex min-h-11 max-w-full items-center gap-2 text-sm sm:min-h-7">
          <label htmlFor={fieldId}>{label}</label>
          {field}
        </span>
      ) : (
        <button
          aria-describedby={describedBy}
          className={`${FIELD_CHIP} ${textSecondaryHoverToPrimary} outline-none focus-visible:ring-2 ${focusVisibleRing}`}
          onClick={() => setOpened(true)}
          type="button"
        >
          <span className="font-normal">{label}</span>
          <span>{draft === "" ? placeholder : draft}</span>
        </button>
      )}
      {/* The suggestions of an open box: a chip draws no input that could offer them. */}
      {listId === undefined || (closed !== undefined && !opened) ? null : (
        <datalist id={listId}>
          {(options ?? []).map((option) => (
            <option key={option} value={option} />
          ))}
        </datalist>
      )}
      {placed}
    </>
  );
}
