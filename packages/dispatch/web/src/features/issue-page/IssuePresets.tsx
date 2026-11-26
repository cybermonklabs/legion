import { type KeyboardEvent, type ReactNode, useEffect, useId, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import {
  borderTransparent,
  focusVisibleRing,
  pageDecisionText,
  pageFailedText,
  pageHairBorder,
  pageHeaderControl,
  pageInkBorder,
  pageInkText,
  pageMenu,
  pageMenuItem,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
} from "../../theme/classes";
import { dialogOpen, useMediaQuery } from "../shell/useDialog";
import { FILTER_ROW_ATTRIBUTE, HEADER_SCROLLS_QUERY } from "./identity-block";
import { presetPath } from "./issue-address";
import { authorText, pageChrome } from "./page-structure";
import {
  CONTROL_LABELS,
  FILTER_SIGNAL_WORDS,
  PRESET_NAMES,
  PRESET_ORDER,
  type Preset,
} from "./page-words";
import { type Filter, isFilter, type PresetSignal } from "./presets";
import { useShortcutHelpOpener } from "./shortcut-help-opener";

/**
 * The view row (R2, R11, R20, and the prototype's view tabs): five
 * controls, Conversation - the page with no view on - then Spec, Verification, Plan and
 * Decisions; the four filters draw signals and Conversation does not. Keys sits at the right end.
 *
 * The row is one line at every width (the architect's ruling 5, the 1440 and 390 px pictures).
 * At 760 px and below the row is the one control and Outline,
 * which stay at the top of the screen while the header scrolls away (`identity-block.ts` finds
 * the row by `FILTER_ROW_ATTRIBUTE`), and Keys is not drawn.
 *
 * One view is on at a time or none. The row is a toolbar of toggle buttons, each carrying
 * `aria-pressed`: pressing a view turns it on and any other off, pressing the view that is on
 * turns it off, and pressing Conversation turns every view off. Each control's state is an
 * address of its own (`issue-address.ts`'s `presetPath`), so pressing one is navigation and a
 * view survives a reload and a link. The toolbar is one tab stop, the control that is pressed;
 * the arrow keys, Home and End move between the five, and Enter and Space press.
 *
 * A toggle is the prototype's `.tab`: its name, which is the button, named by that word alone -
 * the word a voice user says. A filter's signal is beside the button in the same box, as a
 * `<label>` so a press on the count presses the button too. The signal is the button's
 * description (`aria-describedby`), not a word drawn inside the button and missing from its
 * name (WCAG 2.5.3). Each filter holds its signal's room from its first draw, empty until the
 * counts are read (`Signal`), so no control moves when they arrive.
 *
 * The row has two shapes and no third. From 1280 px it is the row of five, which is where R2
 * says the views are one row; below it - R2's 1024 px and every width between, where the five
 * and the small controls measured up to 47 px wider than the row at 1025 px with a view on - it
 * is one control naming the one on, with its signal if drawn, which opens a menu of the five, each a
 * `menuitemradio` that says whether it is the one on; below 1024 px the Outline control stands
 * beside it (R2, R14). One query and its negation decide the two (`ONE_CONTROL_QUERY`), so no
 * width answers both shapes or neither, and the row is one line at every width. The one control
 * is a WAI-ARIA menu button: Enter, Space, Down and Up open its menu with the focus on the view
 * that is on; Up and Down move in it, wrapping, and Home and End go to its ends; Escape closes it
 * and gives the focus back to the control, and Tab closes it.
 */
export function IssuePresets({
  active,
  issueKey,
  outline,
  signals,
}: {
  active: Preset;
  issueKey: string;
  /** The Outline control, beside the one control below 1024 px where the Contents pane is not
   *  drawn (R14), and undefined wherever no Outline control is drawn. */
  outline?: ReactNode;
  /** What each control draws after its name (`presets.ts`'s `filterSignals`). Conversation draws
   *  no signal. The whole record is undefined until the counts can be read
   *  (`slots/view-row.tsx`), and until then each filter holds its signal's room empty. */
  signals?: Readonly<Record<Filter, PresetSignal>>;
}): ReactNode {
  const navigate = useNavigate();
  const { pathname } = useLocation();
  const openKeys = useShortcutHelpOpener();
  // The row of five is drawn exactly where the one control is not.
  const row = !useMediaQuery(ONE_CONTROL_QUERY);
  // Where the header scrolls away, the row holds the one control and Outline alone.
  const phone = useMediaQuery(HEADER_SCROLLS_QUERY);
  // The page opens at most one menu at a time, so which one is open is one value.
  const [open, setOpen] = useState<OpenMenu>(undefined);
  const wrapper = useRef<HTMLDivElement>(null);
  const toggles = useRef<Partial<Record<Preset, HTMLButtonElement | null>>>({});
  // The one control, a menu button (WAI-ARIA's pattern): its menu opens with the focus on the view
  // that is on, and the control takes the focus back when the menu closes with the focus in it -
  // on a chosen view or on Escape - since the item the focus was on goes with the menu. Tab
  // leaves the menu from the control (`leftByTab`), so the focus stays where Tab moved it.
  const chooser = useRef<HTMLButtonElement>(null);
  const wasOpen = useRef<OpenMenu>(undefined);
  const leftByTab = useRef(false);
  // The ids a control's name and signal are read by; one prefix per drawn row, since two issue
  // pages in one document would otherwise write the same ids.
  const ids = useId();
  const nameId = (preset: Preset) => `${ids}-name-${preset}`;
  const signalId = (preset: Preset) => `${ids}-signal-${preset}`;
  const toggleId = (preset: Preset) => `${ids}-toggle-${preset}`;

  useEffect(() => {
    const opened = wasOpen.current === undefined && open === "presets";
    const closed = wasOpen.current === "presets" && open === undefined;
    wasOpen.current = open;
    if (opened) {
      wrapper.current
        ?.querySelector<HTMLElement>('[role="menuitemradio"][aria-checked="true"]')
        ?.focus();
      return;
    }
    if (!closed) return;
    if (leftByTab.current) {
      leftByTab.current = false;
      return;
    }
    const focus = document.activeElement;
    if (focus === null || focus === document.body || wrapper.current?.contains(focus) === true) {
      chooser.current?.focus();
    }
  }, [open]);

  useEffect(() => {
    if (open === undefined) return;
    // A dialog drawn over the row - the Outline sheet - takes its own presses and its own
    // Escape: the row's menu waits for it.
    const inDialog = (target: EventTarget | null) =>
      target instanceof Element && target.closest('[role="dialog"]') !== null;
    const closeOutside = (event: PointerEvent) => {
      if (inDialog(event.target)) return;
      if (!(event.target instanceof Node) || wrapper.current?.contains(event.target) !== true) {
        setOpen(undefined);
      }
    };
    // The press is the open menu's alone: the page's own Escape - Details' among them - waits for
    // the next one. The menu reads it in the capture phase, so it marks the press before any
    // listener of the page's that a layer under it added earlier. A dialog open over the row
    // takes the press wherever the focus is: on the dialog itself in its first frame, on its
    // field, or gone to the page after a press on its text. Whether one is open is `useDialog`'s
    // registry's answer (`dialogOpen()`), which also decides which dialog the key closes, as
    // Details' drawer asks it.
    const closeOnEscape = (event: globalThis.KeyboardEvent) => {
      if (event.key !== "Escape" || inDialog(event.target)) return;
      if (dialogOpen()) return;
      event.preventDefault();
      setOpen(undefined);
    };
    document.addEventListener("pointerdown", closeOutside);
    document.addEventListener("keydown", closeOnEscape, true);
    return () => {
      document.removeEventListener("pointerdown", closeOutside);
      document.removeEventListener("keydown", closeOnEscape, true);
    };
  }, [open]);

  // A press on a view turns it on, or off where it is the one on; Conversation turns every view
  // off, and is a press that changes nothing only on the issue's own address, which is
  // Conversation's: from any other address of the issue - `/collaborators` among them - it goes
  // to the issue's own.
  const press = (preset: Preset) => {
    if (preset === "conversation" && pathname === presetPath(issueKey, "conversation")) return;
    navigate(
      presetPath(issueKey, preset === active && preset !== "conversation" ? "conversation" : preset)
    );
  };
  const walk = (event: KeyboardEvent<HTMLElement>, from: Preset) => {
    const at = PRESET_ORDER.indexOf(from);
    const to = step(event.key, at, PRESET_ORDER.length, "ArrowLeft", "ArrowRight");
    const next = to === undefined ? undefined : PRESET_ORDER[to];
    if (next === undefined) return;
    event.preventDefault();
    toggles.current[next]?.focus();
  };
  // Inside the open menu, Up and Down move between the five, wrapping, and Home and End go to
  // the ends. Tab closes it: the focus goes back to the control first, so Tab moves on from the
  // control, and the items take no tab stop of their own.
  const walkMenu = (event: KeyboardEvent<HTMLElement>) => {
    if (event.key === "Tab") {
      leftByTab.current = true;
      chooser.current?.focus();
      setOpen(undefined);
      return;
    }
    const items = [...event.currentTarget.querySelectorAll<HTMLElement>('[role="menuitemradio"]')];
    const at = event.target instanceof HTMLElement ? items.indexOf(event.target) : -1;
    const to = at < 0 ? undefined : step(event.key, at, items.length, "ArrowUp", "ArrowDown");
    if (to === undefined) return;
    event.preventDefault();
    items[to]?.focus();
  };
  // The control the toolbar's one tab stop rests on, which the arrow keys move from.
  const [stop, setStop] = useState<Preset>(active);
  useEffect(() => setStop(active), [active]);
  // Keys lists the keys as they stand where the reader reads: the outline's own keys are offered
  // while the focus is in the outline, so the focus goes to the outline's tab stop before the
  // list is taken, rather than the list greying every outline key as the button's.
  const showKeys = () => {
    document
      .querySelector<HTMLElement>(
        `[role="tree"][aria-label="${CONTROL_LABELS.tree}"] [data-node-row][tabindex="0"]`
      )
      ?.focus({ preventScroll: true });
    openKeys?.();
  };

  const keys =
    openKeys === undefined ? null : (
      <button className={pageHeaderControl} onClick={showKeys} type="button" {...pageChrome}>
        {CONTROL_LABELS.keys}
      </button>
    );
  // The small controls, at the row's right end from 761 px up.
  const atTheEnd = <div className="flex shrink-0 items-center gap-2">{keys}</div>;
  const filterRow = { [FILTER_ROW_ATTRIBUTE]: "" };

  if (!row) {
    return (
      // The prototype's `#viewbar`: the one control and Outline on one line, 8 px above and
      // below, and from 761 px the small controls at its right end.
      <div className="flex min-w-0 items-center gap-2 py-2" ref={wrapper} {...filterRow}>
        {/* Named by what it draws - "Spec 21" - since the signal is inside this control and
            nothing holds it to a name of its own (WCAG 2.5.3). */}
        <div
          className="relative flex min-w-0 max-[761px]:flex-1 min-[761px]:shrink"
          data-testid="issue-presets"
        >
          <button
            aria-expanded={open === "presets"}
            aria-haspopup="menu"
            className={`flex min-w-0 flex-1 items-center justify-between gap-2 rounded-lg border px-3 py-1.5 text-sm leading-5 font-semibold ${pageHairBorder} ${pagePaperBg} ${pageInkText} ${focusVisibleRing}`}
            onClick={() => setOpen((shown) => (shown === "presets" ? undefined : "presets"))}
            // Down and Up open the menu as Enter and Space do.
            onKeyDown={(event) => {
              if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
              event.preventDefault();
              setOpen("presets");
            }}
            // As a toggle's press does (below), the press leaves the focus where it is; the
            // menu takes it once it opens.
            onMouseDown={(event) => event.preventDefault()}
            ref={chooser}
            type="button"
          >
            <span className="flex min-w-0 items-baseline gap-1.5">
              <span data-preset-name="" {...pageChrome}>
                {PRESET_NAMES[active]}
              </span>
              {isFilter(active) ? <Signal preset={active} signal={signals?.[active]} /> : null}
            </span>
            <MenuMark />
          </button>
          {open === "presets" ? (
            <div className={`${pageMenu} left-0`} onKeyDown={walkMenu} role="menu">
              {PRESET_ORDER.map((preset) => {
                const signal = isFilter(preset) ? signals?.[preset] : undefined;
                return (
                  // Named by its name and any signal it draws; checked where it is the one on.
                  <button
                    aria-checked={preset === active}
                    aria-labelledby={
                      signal === undefined
                        ? nameId(preset)
                        : `${nameId(preset)} ${signalId(preset)}`
                    }
                    className={`${pageMenuItem} flex items-baseline gap-1.5`}
                    key={preset}
                    onClick={() => {
                      setOpen(undefined);
                      if (preset !== active || preset === "conversation") press(preset);
                    }}
                    role="menuitemradio"
                    tabIndex={-1}
                    type="button"
                  >
                    <span aria-hidden="true" className={`w-3 shrink-0 ${pageInkText}`}>
                      {preset === active ? "●" : ""}
                    </span>
                    <span id={nameId(preset)} {...pageChrome}>
                      {PRESET_NAMES[preset]}
                    </span>
                    {isFilter(preset) ? (
                      <Signal id={signalId(preset)} preset={preset} signal={signal} />
                    ) : null}
                  </button>
                );
              })}
            </div>
          ) : null}
        </div>
        {/* After the one control, as the 390 px picture draws it. */}
        {outline}
        {phone ? null : (
          <>
            <span className="flex-1" />
            {atTheEnd}
          </>
        )}
      </div>
    );
  }

  return (
    <div className="flex min-w-0 items-end gap-x-2" ref={wrapper} {...filterRow}>
      <div
        aria-label={CONTROL_LABELS.presets}
        className="flex min-w-0 items-end gap-0.5"
        data-testid="issue-presets"
        role="toolbar"
      >
        {PRESET_ORDER.map((preset) => {
          const on = preset === active;
          const signal = isFilter(preset) ? signals?.[preset] : undefined;
          return (
            // The prototype's `.tab`: the name and any signal on one baseline, the rule under
            // both, ink and semibold for the one on.
            <span
              className={`-mb-px inline-flex shrink-0 items-baseline whitespace-nowrap border-b-2 text-sm leading-5 ${
                on
                  ? `font-semibold ${pageInkText} ${pageInkBorder}`
                  : `${borderTransparent} font-medium ${pageMutedText}`
              }`}
              key={preset}
            >
              <button
                aria-describedby={signal === undefined ? undefined : signalId(preset)}
                aria-labelledby={nameId(preset)}
                aria-pressed={on}
                className="pt-[7px] pb-1.5 pl-3 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-page-ink"
                id={toggleId(preset)}
                onClick={() => press(preset)}
                onFocus={() => setStop(preset)}
                onKeyDown={(event) => walk(event, preset)}
                // A press leaves the focus where it is. Focus taken on mousedown leaves the
                // outline, which closes the body its row had open: on an address that names a
                // node (`#b-`) the page grows shorter under the pointer, the press ends on
                // something else, and the view does not change.
                onMouseDown={(event) => event.preventDefault()}
                ref={(node) => {
                  toggles.current[preset] = node;
                }}
                tabIndex={preset === stop ? 0 : -1}
                type="button"
              >
                <span data-preset-name="" id={nameId(preset)} {...pageChrome}>
                  {PRESET_NAMES[preset]}
                </span>
              </button>
              {/* Its press leaves the focus where it is too, as its button's does. A label
                  takes no focus, so its mousedown would move the focus to the body and out of
                  the outline; and its own activation focuses the button before pressing it. So
                  the label holds the mousedown, and its click presses the view itself in place
                  of that activation. */}
              {/* biome-ignore lint/a11y/useKeyWithClickEvents: the button beside it is the
                  keyboard's way in; the label takes no focus, so no key reaches it. */}
              <label
                className="cursor-pointer pt-[7px] pr-3 pb-1.5 pl-1.5"
                htmlFor={toggleId(preset)}
                onClick={(event) => {
                  event.preventDefault();
                  press(preset);
                }}
                onMouseDown={(event) => event.preventDefault()}
              >
                {isFilter(preset) ? (
                  <Signal id={signalId(preset)} preset={preset} signal={signal} />
                ) : null}
              </label>
            </span>
          );
        })}
      </div>
      <span className="flex-1" />
      <div className="mb-[7px]">{atTheEnd}</div>
    </div>
  );
}

/** The one control's ▾, drawn one em square so it takes the size of the control's text, in its
 *  colour. It is a shape and not text, as `StatusGlyph`'s are, because the page's typeface lacks
 *  ▾: as text, it is drawn in a face of the reader's own machine, at that face's width. The
 *  control's `aria-haspopup` says it opens a menu, so the shape is hidden from assistive
 *  technology. */
export function MenuMark(): ReactNode {
  return (
    <svg
      aria-hidden="true"
      className="inline-block size-[1em] shrink-0 align-middle"
      focusable="false"
      viewBox="0 0 16 16"
    >
      <path d="M4.5 6.5h7L8 10.5z" fill="currentColor" />
    </svg>
  );
}

/** Which of the row's menus is open; the page opens at most one at a time. */
type OpenMenu = "presets" | undefined;

/** The widths the row is one control at: below 1280 px, from which R2 has the views one row.
 *  1279 px, not `80rem`: the row's widths are the reader's pixels, as the header's are. */
const ONE_CONTROL_QUERY = "(max-width: 1279px)";

/** Where a roving focus goes from `at` of `count` controls on `key`: `back` and `forward` step
 *  one, wrapping, and Home and End go to the ends; undefined for any other key. */
function step(
  key: string,
  at: number,
  count: number,
  back: string,
  forward: string
): number | undefined {
  if (key === forward) return (at + 1) % count;
  if (key === back) return (at - 1 + count) % count;
  if (key === "Home") return 0;
  if (key === "End") return count - 1;
  return undefined;
}

/**
 * The room each filter's signal takes from its control's first draw, in the signal's own `ch`,
 * the advance of "0" in its 12 px medium face: the counts arrive seconds after the row on a large
 * issue (`slots/view-row.tsx`), and a control that grew then moved every control after it under a
 * press aimed at one of them. Each room holds its filter's longest signal (`PresetSignal`) with a
 * count of three digits and a flag of two. In the page's Inter, where "0" is 7.57 px at 12 px and
 * no digit is wider, "000" is 3.00 ch, "000 steps" 7.53 ch, "000 · 00 failing" 11.33 ch and
 * "000 · 00 to review" 13.73 ch; each room rounds that up, which leaves room for the medium face
 * the signal is drawn in. A longer signal still widens its control.
 *
 * With every room drawn the row of five is 732 px: each control its 12 px before the name, the
 * name (14 px: Conversation 88 px, Spec 34, Verification 76, Plan 28, Decisions 64), the label's
 * 6 px and 12 px and the room (Spec 23 px, Verification 91, Plan 61, Decisions 110), and 2 px
 * between controls. With Keys (50 px) and the row's two 8 px gaps it needs 798 px, which the
 * 1168 px the row has at 1280 px (less the 56 px rail and the 28 px insets) and the 1328 px it has
 * at 1440 px hold on one line. Below 1280 px the one control's widest name and room, Decisions',
 * take 230 px of the 362 px it has at 390 px; the menu is at least 260 px wide (`pageMenu`), and
 * its widest item, Decisions with its room, takes 214 px of the 246 px inside its padding and
 * rule, so no item widens it when the signals arrive.
 */
const SIGNAL_ROOM: Readonly<Record<Filter, string>> = {
  decisions: "min-w-[14.5ch]",
  plan: "min-w-[8ch]",
  spec: "min-w-[3ch]",
  verification: "min-w-[12ch]",
};

/**
 * What one filter control draws after its name, in its room (`SIGNAL_ROOM`): its count, then the
 * noun the count is in, or what of it asks something of the reader, in the prototype's `.sig`
 * (12 px, the quiet grey; the failed red for what failed and the decision violet for what waits
 * for review, each with its word, so the colour repeats what the word says). The count is the
 * author's number and carries `data-preset-count`, which the checks read it by. Until the signal
 * is known the room is drawn empty and hidden from assistive technology, so it holds the
 * control's width and no screen reader hears it or find-in-page matches in it. The room carries
 * `data-preset-signal` either way.
 */
function Signal({
  id,
  preset,
  signal,
}: {
  id?: string;
  preset: Filter;
  signal: PresetSignal | undefined;
}): ReactNode {
  return (
    <span
      aria-hidden={signal === undefined ? true : undefined}
      className={`inline-block ${SIGNAL_ROOM[preset]} text-xs leading-4 font-medium ${pageQuietText}`}
      data-preset-signal={preset}
      id={id}
    >
      {signal === undefined ? null : (
        <>
          <span data-preset-count={preset} {...authorText}>
            {signal.count}
          </span>
          {signal.unit === undefined ? null : (
            <span {...pageChrome}> {FILTER_SIGNAL_WORDS[signal.unit]}</span>
          )}
          {signal.flag === undefined ? null : (
            <span
              className={signal.flag.word === "failing" ? pageFailedText : pageDecisionText}
              data-preset-flag={signal.flag.word}
              {...pageChrome}
            >
              {" · "}
              <span {...authorText}>{signal.flag.count}</span>{" "}
              {FILTER_SIGNAL_WORDS[signal.flag.word]}
            </span>
          )}
        </>
      )}
    </span>
  );
}
