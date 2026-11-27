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

import { focusBackOverPress, focusOnDocument, stepIndex } from "../features/shell/roving";
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

/** One choice of a menu: an action. */
export interface MenuChoice<V extends string> {
  readonly value: V;
  readonly label: string;
}

interface ChoiceMenuProps<V extends string> {
  /** The menu's accessible name, and the first words of its trigger's: "Move <column>". */
  readonly label: string;
  readonly choices: readonly MenuChoice<V>[];
  readonly onChoose: (value: V) => void;
  /** The trigger's marks, by which the page's code finds it (`data-column-move`). */
  readonly marks: Readonly<Record<`data-${string}`, string>>;
  readonly triggerClassName: string;
  /** The words the trigger shows, which `children` draws: the action of the menu ("Move"). */
  readonly face: string;
  /** What the trigger draws: the icon or the badge that shows `face`. */
  readonly children: ReactNode;
}

/** A press inside the trigger or the menu stays there: what the control sits in - a draggable
 *  card or header cell, a row that selects itself on a click - may not see the press that opens
 *  or picks. The menu is portalled to the body, but it is a React child of the control, so its
 *  presses would bubble to the same element. */
const keepPress = (event: SyntheticEvent) => event.stopPropagation();

/**
 * A control whose press opens a menu of actions, in the menu button pattern of the WAI-ARIA
 * Authoring Practices: a table column's Move left and Move right (`DataTable`). A list of values
 * a reader picks from - a snooze, a priority, a status - is the shared single pick instead
 * (`MultiSelect`), whose text box narrows its values (dispatch://INFRA-2160, D6).
 *
 * The menu opens with its first choice highlighted and focused, and the arrows, Home and End
 * move the highlight and write nothing. Enter writes the highlighted choice, and a click or a tap
 * writes the one it hits. Escape, Tab and a press outside close the menu and write nothing. A
 * pick closes the menu, hands the focus back, then writes, so a write that takes the control off
 * the page takes it from where the reader came from.
 *
 * The focus goes back where it was before the menu opened. The trigger's click is where that is
 * read: a click with no pointer (`detail` 0) is a key's - the page's binding pressing the
 * trigger from a row, or Enter or Space on the trigger - and the focus is still where the key
 * was pressed; a pointer's click is on the trigger, which the focus goes back to. It goes back to
 * the trigger too where nothing had the focus as the key was pressed (`focusOnDocument()`), and
 * where that element has left the page since.
 *
 * A press outside closes the menu in the document's capture phase, before any control under the
 * press can stop it, and hands the focus back there. The browser's own focus on a press is the
 * default action of its `mousedown`, which comes after, so a press on a text box, a button or a
 * link then focuses that control and acts on that one press; a press on nothing that takes the
 * focus leaves it on the document, and the menu hands it back again once the press is over.
 *
 * A foot line names the menu's keys. The choices are 44 px high at every width, the height of a
 * tap, and drawn in the theme's colours (R10). The panel is `AnchoredPanel`, so the menu stands
 * over every layer and its keys are its own (`PANEL_ATTRIBUTE`).
 */
export function ChoiceMenu<V extends string>({
  children,
  choices,
  face,
  label,
  marks,
  onChoose,
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
  // Where the focus goes back to as the menu closes, recorded as it opens.
  const returnTo = useRef<HTMLElement | null>(null);
  // The element a pick handed the focus back to, followed until the reader's next act (`follow`).
  const followed = useRef<HTMLElement | null>(null);
  const unfollow = useRef<() => void>(() => undefined);
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
   *  moves the focus: the pick's write can move it on the page - a column's move draws its header
   *  cell, with the trigger, at the column's new place - and the focus it holds is kept in sight
   *  as it moves, so a later going of that element hands the focus on rather than reading as one
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
    setActive(0);
    setOpen(true);
  };

  // The menu opens on its first choice.
  useEffect(() => {
    if (open) choiceRefs.current[0]?.focus();
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
  // their own presses from bubbling, and a press on either must still close this menu. The focus
  // goes back at once and again once the press is over where it left it on the document
  // (`focusBackOverPress`), each time with no scroll, as the shared single pick gives it back
  // (`MultiSelect`'s `handBackOverPress`): a scroll then would move the page under the press
  // between its `mousedown` and its `mouseup`, so a press on a control far from the trigger would
  // release on another element and not act.
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
      focusBackOverPress(() => back?.focus({ preventScroll: true }));
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
    if (event.key !== "Enter") return;
    const choice = choices[index];
    if (choice === undefined) return;
    event.preventDefault();
    choose(choice);
  };

  // The trigger is named for its menu and then for what it shows, so the words on it, the ones a
  // sighted reader reads and a speech-input reader says to press it, are in its name (WCAG 2.5.3,
  // Label in Name), as a single pick's trigger is named (`MultiSelect`). The menu's name is a
  // hidden element beside the trigger, outside it so the trigger's text stays the words it
  // shows. A trigger whose face is the first words of its label already - "Move" of "Move
  // Status" - is named by its label alone.
  const faceInLabel = label === face || label.startsWith(`${face} `);
  return (
    <>
      {faceInLabel ? null : (
        <span hidden id={nameId}>
          {label}
        </span>
      )}
      <button
        aria-controls={open ? menuId : undefined}
        aria-expanded={open}
        aria-haspopup="menu"
        aria-label={faceInLabel ? label : undefined}
        aria-labelledby={faceInLabel ? undefined : `${nameId} ${faceId}`}
        className={triggerClassName}
        {...marks}
        onClick={(event) => {
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
                role="menuitem"
                tabIndex={index === highlighted ? 0 : -1}
                type="button"
              >
                <span className="min-w-0 flex-1 truncate">{choice.label}</span>
              </button>
            ))}
            <div
              aria-hidden="true"
              className={`mt-1 border-t px-3 pt-1.5 pb-0.5 text-xs ${borderDefault} ${textMutedOnSurface}`}
            >
              ↑ ↓ Home End to move · Enter to choose · Esc to close
            </div>
          </div>
        </AnchoredPanel>
      ) : null}
    </>
  );
}
