import {
  type KeyboardEvent,
  type MouseEvent,
  type ReactNode,
  type SyntheticEvent,
  useEffect,
  useId,
  useRef,
  useState,
} from "react";

import { focusOnDocument, stepIndex } from "../features/shell/roving";
import { ControlKeyHint, digitKbdClass, keyshortcutsOf } from "../features/shell/ShortcutHelp";
import {
  borderDefault,
  card,
  focusVisibleRing,
  selectedCardBg,
  surfaceMutedHoverBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../theme/classes";
import { AnchoredPanel } from "./AnchoredPanel";

/** One choice of a menu. */
export interface MenuChoice<V extends string> {
  readonly value: V;
  readonly label: string;
  /** The digit that writes this choice at once while the menu is open, drawn beside it. */
  readonly digit?: string;
}

interface ChoiceMenuProps<V extends string> {
  /** The menu's accessible name, and the first words of its trigger's: "Snooze <name>",
   *  "Priority of <KEY>", "Status". */
  readonly label: string;
  readonly choices: readonly MenuChoice<V>[];
  /** The value a menu of values holds now - a priority, a status. Given, its choices are radios
   *  (`menuitemradio`), it opens on this one, and choosing this value again writes nothing.
   *  Absent, the choices are actions (a snooze), and the menu opens on the first. */
  readonly checked?: V;
  readonly onChoose: (value: V) => void;
  /** The trigger takes no press and no focus: a closed issue, a card in Done. */
  readonly disabled?: boolean;
  /** The trigger keeps its focus and its Tab stop but opens nothing (`aria-disabled`): a write of
   *  its own is out, and a control disabled under the reader's focus drops it on the document. */
  readonly held?: boolean;
  /** The trigger's marks, by which the keys find it and press it (`data-inbox-snooze`,
   *  `data-inbox-bulk-snooze`, `data-priority-menu`, `data-status-menu`). */
  readonly marks: Readonly<Record<`data-${string}`, string>>;
  readonly triggerClassName: string;
  /** The keys that also press the trigger where its host binds them and draws them (the
   *  Inbox's `b` and `p`, in the reading pane's head): named in the trigger's
   *  `aria-keyshortcuts`, and the first drawn inside it after what it draws (`ControlKeyHint`). */
  readonly shortcutKeys?: readonly string[];
  /** The words the trigger shows, which `children` draws: the value a menu of values holds
   *  ("P1", "In progress"), or the action of a menu of actions ("Snooze", "Snoozing…"). */
  readonly face: string;
  /** What the trigger draws: the badge or the pill that shows `face`. */
  readonly children: ReactNode;
}

/** A press inside the trigger or the menu stays there: a board card is a drag activator, and an
 *  Inbox row selects itself on a click, so neither may see the press that opens or picks. The menu
 *  is portalled to the body, but it is a React child of the control, so its presses would bubble
 *  to the same card. */
const keepPress = (event: SyntheticEvent) => event.stopPropagation();

/** The digits a menu takes, as its foot line names them: a run ("1–4", "0–3"), else each. */
function digitsText(choices: readonly MenuChoice<string>[]): string | undefined {
  const digits = choices
    .flatMap((choice) => (choice.digit === undefined ? [] : [Number(choice.digit)]))
    .sort((a, b) => a - b);
  const first = digits[0];
  const last = digits.at(-1);
  if (first === undefined || last === undefined) return undefined;
  if (first === last) return `${first}`;
  return digits.every((digit, index) => digit === first + index)
    ? `${first}–${last}`
    : digits.join(" ");
}

/**
 * A control whose press opens a menu of its choices, in the menu button pattern of the WAI-ARIA
 * Authoring Practices: the snooze, the priority and the status. A key that names the control
 * presses its trigger (`b` and `h`, `p`, `s`), so a key, a click and a tap all open the same menu.
 *
 * The menu opens with a choice highlighted and focused - the checked one, else the first - and
 * the arrows, Home and End move the highlight and write nothing. Enter writes the highlighted
 * choice, a click or a tap writes the one it hits, and a digit writes its choice at once, with
 * `preventDefault`, so the page's keymap, which skips a handled key, never reads it as its own.
 * Escape, Tab and a press outside close the menu and write nothing. A pick closes the menu,
 * hands the focus back, then writes, so a write that takes the control off the page - a snooze
 * that folds its row - takes it from where the reader came from.
 *
 * The focus goes back where it was before the menu opened. The trigger's click is where that is
 * read: a click with no pointer (`detail` 0) is a key's - the page's binding pressing the
 * trigger from a row, or Enter or Space on the trigger - and the focus is still where the key
 * was pressed; a pointer's click is on the trigger, which the focus goes back to. It goes back to
 * the trigger too where nothing had the focus as the key was pressed (`focusOnDocument()`, the
 * Inbox's `b` pressed on the page), and where that element has left the page since.
 *
 * A press outside closes the menu in the document's capture phase, before any control under the
 * press can stop it, and hands the focus back there. The browser's own focus on a press is the
 * default action of its `mousedown`, which comes after, so a press on a text box, a button or a
 * link then focuses that control and acts on that one press; a press on nothing that takes the
 * focus leaves it on the document, and the menu hands it back again once the press is over.
 *
 * Each choice draws its digit, and a foot line names the menu's keys. The choices are 44 px high
 * at every width, the height of a tap, and drawn in the theme's colours (R10). The panel is
 * `AnchoredPanel`, so the menu stands over every layer and its keys are its own
 * (`PANEL_ATTRIBUTE`).
 */
export function ChoiceMenu<V extends string>({
  checked,
  children,
  choices,
  disabled = false,
  face,
  held = false,
  label,
  marks,
  onChoose,
  shortcutKeys,
  triggerClassName,
}: ChoiceMenuProps<V>): ReactNode {
  const id = useId();
  const menuId = `${id}-menu`;
  const nameId = `${id}-name`;
  const faceId = `${id}-face`;
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const choiceRefs = useRef<(HTMLButtonElement | null)[]>([]);
  // The choice the menu opens on, read by the effect that focuses it as the menu opens.
  const opensOn = useRef(0);
  // Where the focus goes back to as the menu closes, recorded as it opens.
  const returnTo = useRef<HTMLElement | null>(null);
  // The element a pick handed the focus back to, followed until the reader's next act (`follow`).
  const followed = useRef<HTMLElement | null>(null);
  const unfollow = useRef<() => void>(() => undefined);
  const radio = checked !== undefined;
  const highlighted = Math.min(active, choices.length - 1);

  /** Where the focus goes back to: where it was as the menu opened, or the trigger where that
   *  element has left the page since. */
  const wayBack = (): HTMLElement | null => {
    const recorded = returnTo.current;
    return recorded?.isConnected ? recorded : triggerRef.current;
  };
  const close = () => {
    setOpen(false);
    wayBack()?.focus();
  };
  /** Follows the element `close` handed the focus back to, until the reader presses, wheels or
   *  moves the focus: the pick's write can move it on the page - a snooze takes its row down into
   *  Later, a priority sorts a list or a board anew - and the focus it holds is kept in sight as
   *  it moves, so a later going of that element hands the focus on rather than reading as one
   *  the reader scrolled away from (`focus-place.ts`'s `outOfSight`). */
  const follow = (element: HTMLElement | null) => {
    unfollow.current();
    followed.current = element;
    if (element === null) return;
    const stop = () => {
      followed.current = null;
      for (const type of ["keydown", "pointerdown", "wheel"]) {
        document.removeEventListener(type, stop, true);
      }
      unfollow.current = () => undefined;
    };
    for (const type of ["keydown", "pointerdown", "wheel"]) {
      document.addEventListener(type, stop, true);
    }
    unfollow.current = stop;
  };
  const choose = (choice: MenuChoice<V>) => {
    close();
    if (choice.value === checked) return;
    // The key or the click that picked is past the document's capture phase already, so it is
    // not the act that ends the following.
    follow(wayBack());
    onChoose(choice.value);
  };
  const highlight = (index: number) => {
    setActive(index);
    choiceRefs.current[index]?.focus();
  };
  const openFrom = (event: MouseEvent<HTMLButtonElement>) => {
    const focused = document.activeElement;
    returnTo.current =
      event.detail === 0 && focused instanceof HTMLElement && !focusOnDocument()
        ? focused
        : event.currentTarget;
    opensOn.current = Math.max(
      choices.findIndex((choice) => choice.value === checked),
      0
    );
    setActive(opensOn.current);
    setOpen(true);
  };

  useEffect(() => {
    if (open) choiceRefs.current[opensOn.current]?.focus();
  }, [open]);

  // After every commit, the pick's write among them: the followed element, while it holds the
  // focus, scrolled the least that brings it into the window. Passive, so it runs after a list's
  // own keeping of its rows in place (`ViewportAnchor`, in the layout phase).
  useEffect(() => {
    const element = followed.current;
    if (element === null) return;
    if (document.activeElement !== element) {
      unfollow.current();
      return;
    }
    const box = element.getBoundingClientRect();
    if (box.top < 0 || box.bottom > window.innerHeight) {
      element.scrollIntoView?.({ block: "nearest" });
    }
  });
  useEffect(() => () => unfollow.current(), []);

  // The press outside, in the capture phase: a board card's priority and an ask's hand-on stop
  // their own presses from bubbling, and a press on either must still close this menu.
  // biome-ignore lint/correctness/useExhaustiveDependencies: `wayBack` reads only refs, so the listener the menu's opening adds is current for as long as the menu stays open
  useEffect(() => {
    if (!open) return;
    const press = (event: PointerEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (triggerRef.current?.contains(target)) return;
      if (document.getElementById(menuId)?.contains(target)) return;
      setOpen(false);
      const back = wayBack();
      back?.focus();
      // The press's own focus has landed by its `mouseup`, a finger's too: back from the
      // document, and left alone anywhere else. A press the browser cancels (a scroll) moves no
      // focus, and the next press is another press, so either ends the wait.
      const settle = () => {
        stop();
        if (focusOnDocument()) back?.focus();
      };
      const stop = () => {
        document.removeEventListener("mouseup", settle, true);
        document.removeEventListener("pointercancel", stop, true);
        document.removeEventListener("pointerdown", stop, true);
      };
      document.addEventListener("mouseup", settle, true);
      document.addEventListener("pointercancel", stop, true);
      document.addEventListener("pointerdown", stop, true);
    };
    document.addEventListener("pointerdown", press, true);
    return () => document.removeEventListener("pointerdown", press, true);
  }, [open, menuId]);

  const onChoiceKeyDown = (event: KeyboardEvent<HTMLButtonElement>, index: number) => {
    if (event.altKey || event.ctrlKey || event.metaKey) return;
    // The arrows wrap at the ends, as the menu button pattern allows.
    const step = stepIndex(event.key, index, choices.length, "ArrowUp", "ArrowDown");
    if (step !== undefined) {
      event.preventDefault();
      highlight(step);
      return;
    }
    const choice =
      event.key === "Enter" ? choices[index] : choices.find(({ digit }) => digit === event.key);
    if (choice === undefined) return;
    event.preventDefault();
    choose(choice);
  };

  // The trigger is named for its menu and then for what it shows - "Priority of CORE-1 P1",
  // "Status In progress" - so the words on it, the ones a sighted reader reads and a speech-input
  // reader says to press it, are in its name (WCAG 2.5.3, Label in Name), and a screen reader
  // hears the menu and the value it holds, as a single pick's trigger is named (`MultiSelect`).
  // The menu's name is a hidden element beside the trigger, outside it so the trigger's text
  // stays the words it shows. A trigger whose face is the first words of its label already -
  // "Snooze" of "Snooze CORE-1", an unset priority's "Priority" - is named by its label alone.
  const faceInLabel = label === face || label.startsWith(`${face} `);
  const digits = digitsText(choices);
  return (
    <>
      {faceInLabel ? null : (
        <span hidden id={nameId}>
          {label}
        </span>
      )}
      <button
        aria-controls={open ? menuId : undefined}
        aria-disabled={held || undefined}
        aria-expanded={open}
        aria-haspopup="menu"
        aria-keyshortcuts={keyshortcutsOf(shortcutKeys)}
        aria-label={faceInLabel ? label : undefined}
        aria-labelledby={faceInLabel ? undefined : `${nameId} ${faceId}`}
        className={triggerClassName}
        {...marks}
        disabled={disabled}
        onClick={(event) => {
          if (held) return;
          if (open) close();
          else openFrom(event);
        }}
        onMouseDown={keepPress}
        onPointerDown={keepPress}
        onTouchStart={keepPress}
        ref={triggerRef}
        type="button"
      >
        <span className="contents" id={faceId}>
          {children}
        </span>
        <ControlKeyHint keys={shortcutKeys} />
      </button>
      {open ? (
        <AnchoredPanel
          anchorRef={triggerRef}
          className={`max-h-96 w-56 max-w-[calc(100vw-1rem)] overflow-auto rounded-lg border shadow-lg ${card}`}
          focusOnOpen={false}
          id={menuId}
          label={label}
          onDismiss={() => setOpen(false)}
          returnFocus={wayBack}
          role="menu"
        >
          {/* biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the handlers keep a press in the menu out of the card or row the control sits in, and keep the focus on the highlighted choice while the press lands, so the wrapper needs no role of its own. */}
          <div
            className="py-1"
            onMouseDown={(event) => {
              event.preventDefault();
              event.stopPropagation();
            }}
            onPointerDown={keepPress}
            onTouchStart={keepPress}
          >
            {choices.map((choice, index) => (
              <button
                aria-keyshortcuts={choice.digit}
                className={`flex min-h-11 w-full items-center gap-2 px-3 py-2 text-left text-sm outline-none focus-visible:ring-2 focus-visible:ring-inset ${focusVisibleRing} ${textPrimaryOnSurface} ${
                  index === highlighted ? selectedCardBg : surfaceMutedHoverBg
                }`}
                key={choice.value}
                onClick={() => choose(choice)}
                onFocus={() => setActive(index)}
                onKeyDown={(event) => onChoiceKeyDown(event, index)}
                ref={(node) => {
                  choiceRefs.current[index] = node;
                }}
                {...(radio
                  ? { "aria-checked": choice.value === checked, role: "menuitemradio" }
                  : { role: "menuitem" })}
                tabIndex={index === highlighted ? 0 : -1}
                type="button"
              >
                {radio ? (
                  <span aria-hidden="true" className="w-4 shrink-0 text-center">
                    {choice.value === checked ? "✓" : ""}
                  </span>
                ) : null}
                <span className="min-w-0 flex-1 truncate">{choice.label}</span>
                {/* The digit is for the eye; assistive tech hears it as `aria-keyshortcuts`. */}
                {choice.digit === undefined ? null : (
                  <span aria-hidden="true" className="shrink-0">
                    <kbd className={digitKbdClass}>{choice.digit}</kbd>
                  </span>
                )}
              </button>
            ))}
            <div
              aria-hidden="true"
              className={`mt-1 border-t px-3 pt-1.5 pb-0.5 text-xs ${borderDefault} ${textMutedOnSurface}`}
            >
              ↑ ↓ Home End to move · Enter{digits === undefined ? "" : ` or ${digits}`} to choose ·
              Esc to close
            </div>
          </div>
        </AnchoredPanel>
      ) : null}
    </>
  );
}
