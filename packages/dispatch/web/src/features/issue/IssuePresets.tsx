import { type KeyboardEvent, type ReactNode, useEffect, useId, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import type { Artifact } from "../../api/types";
import {
  borderTransparent,
  focusVisibleRing,
  pageDecisionText,
  pageFailedText,
  pageField,
  pageHairBorder,
  pageHeaderControl,
  pageInkBorder,
  pageInkText,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
} from "../../theme/classes";
import { useShortcutHelpOpener } from "../shell/shortcut-help-opener";
import { useMediaQuery } from "../shell/useDialog";
import { FILTER_ROW_ATTRIBUTE, HEADER_SCROLLS_QUERY } from "./identity-block";
import { presetPath } from "./issue-address";
import { MENU_CLASS, MENU_ITEM_CLASS, NodeMenu } from "./NodeMenu";
import { authorText, pageChrome } from "./page-structure";
import {
  CONTROL_LABELS,
  FILTER_SIGNAL_WORDS,
  PRESET_NAMES,
  PRESET_ORDER,
  type Preset,
} from "./page-words";
import type { PresetSignal } from "./presets";

/**
 * The filter row (dispatch://INFRA-1196 R2, R11, R20, and the prototype's view tabs): five
 * controls, Conversation - the page with no filter on - then Spec, Verification, Plan and
 * Decisions, each with its signal; and the node menu (R3), the filter by words (`t f`), the
 * version control while a filter is on or a document is open (R2), and Keys.
 *
 * The row is one line at every width (ruling 5 of dispatch://INFRA-1196/artifact/build-rulings-md,
 * the 1440 and 390 px pictures): the node menu, the filter by words and the version control are
 * each one small control at the row's right end, like Keys. The words box opens when its control
 * is pressed or `t f` is, and stays open while it holds words (`?q=`); the version control's
 * picker, its name-a-version control and its link open from the version control. At 760 px and
 * below those three stand in the Outline sheet (`outline`), and the row is the one control and
 * Outline, which stay at the top of the screen while the header scrolls away (`identity-block.ts`
 * publishes their height, found by `FILTER_ROW_ATTRIBUTE`).
 *
 * One filter is on at a time or none. The row is a toolbar of toggle buttons, each carrying
 * `aria-pressed`: pressing a filter turns it on and any other off, pressing the filter that is
 * on turns it off, and pressing Conversation turns every filter off and shuts whatever the
 * address opened in place of the outline. Each control's state is an address of its own
 * (`issue-address.ts`'s `presetPath`), so pressing one is navigation and a filter survives a
 * reload and a link. The toolbar is one tab stop, the control that is pressed; the arrow keys,
 * Home and End move between the five, and Enter and Space press.
 *
 * A toggle is the prototype's `.tab`: its name, which is the button, named by that word alone -
 * the word a voice user says, which the inventory of the page this one replaced holds each
 * control to - and beside it, outside the button and inside the same drawn box, its signal, a
 * `<label>` of the button, so a press on the count presses it too and the signal is the button's
 * description (`aria-describedby`), not a word drawn inside the button and missing from its name
 * (WCAG 2.5.3).
 *
 * The row has two shapes and no third. From 1280 px it is the row of five, which is where R2
 * says the filters are one row; below it - R2's 1024 px and every width between, where the five
 * and the small controls measured up to 47 px wider than the row at 1025 px with a filter on -
 * it is one control naming the one on, with its signal, which opens a menu of the five, each a
 * `menuitemradio` that says whether it is the one on; below 1024 px the Outline control stands
 * beside it (R2, R14). One query and its negation decide the two (`ONE_CONTROL_QUERY`), so no
 * width answers both shapes or neither, and the row is one line at every width.
 */
export function IssuePresets({
  active,
  artifacts,
  filter,
  isClosed,
  issueKey,
  nodeMenu,
  onFilterChange,
  onLeave,
  outline,
  signals,
  toolbar,
}: {
  active: Preset;
  /** Every artifact of the issue, which the node menu lists and which a title typed into its
   *  New document is held against before the write (R3). */
  artifacts: readonly Artifact[];
  /** What the filter by words holds, which is `?q=`. */
  filter: string;
  /** Whether the issue is closed, which refuses the node menu's upload. */
  isClosed: boolean;
  issueKey: string;
  /** Whether the address opens the node menu, which `/artifacts` does. */
  nodeMenu: boolean;
  onFilterChange: (next: string) => void;
  /** Called before this row leaves the address, so the page records where the reader was. */
  onLeave: () => void;
  /** The Outline control, beside the one control below 1024 px where the Contents pane is not
   *  drawn (R14), handed the controls this row moves into its sheet at 760 px and below - the
   *  version control, the node menu and the filter by words - and undefined above that width,
   *  where they stand at the row's right end. It answers nothing where it draws no control. */
  outline?: (tools: ReactNode | undefined) => ReactNode;
  /** What each control draws after its name (`presets.ts`'s `filterSignals`). */
  signals: Readonly<Record<Preset, PresetSignal>>;
  /** The version control: the filter's own while one is on, or the opened document's; none
   *  with no filter on (R2). */
  toolbar?: ReactNode;
}): ReactNode {
  const navigate = useNavigate();
  const { pathname } = useLocation();
  const openKeys = useShortcutHelpOpener();
  // The row of five is drawn exactly where the one control is not.
  const row = !useMediaQuery(ONE_CONTROL_QUERY);
  // Where the header scrolls away, the small controls are the Outline sheet's.
  const phone = useMediaQuery(HEADER_SCROLLS_QUERY);
  // The page opens at most one menu at a time, so which one is open is one value.
  const [open, setOpen] = useState<OpenMenu>(nodeMenu ? "nodes" : undefined);
  const wrapper = useRef<HTMLDivElement>(null);
  const toggles = useRef<Partial<Record<Preset, HTMLButtonElement | null>>>({});
  // The ids a control's name and signal are read by; one prefix per drawn row, since two issue
  // pages in one document would otherwise write the same ids.
  const ids = useId();
  const nameId = (preset: Preset) => `${ids}-name-${preset}`;
  const signalId = (preset: Preset) => `${ids}-signal-${preset}`;
  const toggleId = (preset: Preset) => `${ids}-toggle-${preset}`;
  const versionPanelId = `${ids}-version`;

  // An address that opens the node menu opens it on arrival, and leaving that address closes it
  // again, so the menu says what the address says.
  useEffect(() => setOpen(nodeMenu ? "nodes" : undefined), [nodeMenu]);
  useEffect(() => {
    if (open === undefined) return;
    // A dialog drawn over the row - the version control's Name dialog, the Outline sheet - takes
    // its own presses and its own Escape: the row's menu waits for it.
    const inDialog = (target: EventTarget | null) =>
      target instanceof Element && target.closest('[role="dialog"]') !== null;
    const closeOutside = (event: PointerEvent) => {
      if (inDialog(event.target)) return;
      if (!(event.target instanceof Node) || wrapper.current?.contains(event.target) !== true) {
        setOpen(undefined);
      }
    };
    // The press is the open menu's alone: the page's own Escape - which turns a filter off, and
    // closes a document opened in place of the outline - waits for the next one.
    const closeOnEscape = (event: globalThis.KeyboardEvent) => {
      if (event.key !== "Escape" || inDialog(event.target)) return;
      event.preventDefault();
      setOpen(undefined);
    };
    document.addEventListener("pointerdown", closeOutside);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("pointerdown", closeOutside);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [open]);

  const go = (preset: Preset) => {
    onLeave();
    navigate(presetPath(issueKey, preset));
  };
  // A press on a filter turns it on, or off where it is the one on; Conversation turns every
  // filter off and shuts what the address opened in place of the outline - a document, the
  // history, the node menu's list - and is a press that changes nothing only on the issue's own
  // address, which is Conversation's.
  const press = (preset: Preset) => {
    if (preset === "conversation" && pathname === presetPath(issueKey, "conversation")) return;
    go(preset === active && preset !== "conversation" ? "conversation" : preset);
  };
  const walk = (event: KeyboardEvent<HTMLElement>, from: Preset) => {
    const at = PRESET_ORDER.indexOf(from);
    const to =
      event.key === "ArrowRight"
        ? (at + 1) % PRESET_ORDER.length
        : event.key === "ArrowLeft"
          ? (at - 1 + PRESET_ORDER.length) % PRESET_ORDER.length
          : event.key === "Home"
            ? 0
            : event.key === "End"
              ? PRESET_ORDER.length - 1
              : undefined;
    const next = to === undefined ? undefined : PRESET_ORDER[to];
    if (next === undefined) return;
    event.preventDefault();
    toggles.current[next]?.focus();
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

  const files = (align: "end" | "start") => (
    <NodeMenu
      align={align}
      artifacts={artifacts}
      isClosed={isClosed}
      issueKey={issueKey}
      onClose={() => setOpen(undefined)}
      onToggle={() => setOpen((shown) => (shown === "nodes" ? undefined : "nodes"))}
      open={open === "nodes"}
    />
  );
  const keys =
    openKeys === undefined ? null : (
      <button className={pageHeaderControl} onClick={showKeys} type="button" {...pageChrome}>
        {CONTROL_LABELS.keys}
      </button>
    );
  // The small controls, at the row's right end from 761 px up.
  const atTheEnd = (
    <div className="flex shrink-0 items-center gap-2">
      {toolbar === undefined ? null : (
        <div className="relative flex shrink-0">
          <button
            aria-controls={versionPanelId}
            aria-expanded={open === "version"}
            className={pageHeaderControl}
            onClick={() => setOpen((shown) => (shown === "version" ? undefined : "version"))}
            type="button"
            {...pageChrome}
          >
            {CONTROL_LABELS.version}
            <span aria-hidden="true">▾</span>
          </button>
          {/* Drawn while it is shut too, so a version the reader picked, a name they typed and
              a comparison they opened outlive the control closing. */}
          <div
            className={`absolute top-full right-0 z-30 mt-1 w-max max-w-[min(480px,calc(100vw-24px))] rounded-[10px] border p-2 shadow-[0_12px_30px_rgba(28,25,23,0.18)] ${pageHairBorder} ${pagePaperBg}`}
            hidden={open !== "version"}
            id={versionPanelId}
          >
            {toolbar}
          </div>
        </div>
      )}
      {files("end")}
      <WordsFilter filter={filter} onFilterChange={onFilterChange} />
      {keys}
    </div>
  );
  // The same three, in the Outline sheet at 760 px and below, each drawn open: the sheet is
  // where the reader went to find them.
  const inTheSheet = (
    <>
      {toolbar}
      {files("start")}
      <WordsFilter filter={filter} inSheet onFilterChange={onFilterChange} />
    </>
  );
  const outlineControl = outline?.(phone ? inTheSheet : undefined);
  const filterRow = { [FILTER_ROW_ATTRIBUTE]: "" };

  if (!row) {
    return (
      // The prototype's `#viewbar`: the one control and Outline on one line, 8 px above and
      // below, and from 761 px the small controls at its right end.
      <div className="flex min-w-0 items-center gap-2 py-2" ref={wrapper} {...filterRow}>
        {/* Named by what it draws - "Spec 21" - since the signal is inside this control and
            nothing holds it to a name of its own (WCAG 2.5.3). */}
        <div
          className="relative flex min-w-0 max-[760px]:flex-1 min-[761px]:shrink"
          data-testid="issue-presets"
        >
          <button
            aria-expanded={open === "presets"}
            aria-haspopup="menu"
            className={`flex min-w-0 flex-1 items-center justify-between gap-2 rounded-lg border px-3 py-1.5 text-sm leading-5 font-semibold ${pageHairBorder} ${pagePaperBg} ${pageInkText} ${focusVisibleRing}`}
            onClick={() => setOpen((shown) => (shown === "presets" ? undefined : "presets"))}
            type="button"
          >
            <span className="flex min-w-0 items-baseline gap-1.5">
              <span data-preset-name="" {...pageChrome}>
                {PRESET_NAMES[active]}
              </span>
              <Signal preset={active} signal={signals[active]} />
            </span>
            <span aria-hidden="true" {...pageChrome}>
              ▾
            </span>
          </button>
          {open === "presets" ? (
            <div className={`${MENU_CLASS} left-0`} role="menu">
              {PRESET_ORDER.map((preset) => (
                // Named by its name and its signal, which it draws inside it; and checked
                // where it is the one on, which its dot draws.
                <button
                  aria-checked={preset === active}
                  aria-labelledby={`${nameId(preset)} ${signalId(preset)}`}
                  className={`${MENU_ITEM_CLASS} flex items-baseline gap-1.5`}
                  key={preset}
                  onClick={() => {
                    setOpen(undefined);
                    if (preset !== active || preset === "conversation") press(preset);
                  }}
                  role="menuitemradio"
                  type="button"
                >
                  <span aria-hidden="true" className={`w-3 shrink-0 ${pageInkText}`}>
                    {preset === active ? "●" : ""}
                  </span>
                  <span id={nameId(preset)} {...pageChrome}>
                    {PRESET_NAMES[preset]}
                  </span>
                  <Signal id={signalId(preset)} preset={preset} signal={signals[preset]} />
                </button>
              ))}
            </div>
          ) : null}
        </div>
        {/* After the one control, as the 390 px picture draws it. */}
        {outlineControl}
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
          return (
            // The prototype's `.tab`: the name and the signal on one baseline, the rule under
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
                aria-describedby={signalId(preset)}
                aria-labelledby={nameId(preset)}
                aria-pressed={on}
                className="pt-[7px] pb-1.5 pl-3 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-page-ink"
                id={toggleId(preset)}
                onClick={() => press(preset)}
                onFocus={() => setStop(preset)}
                onKeyDown={(event) => walk(event, preset)}
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
              <label
                className="cursor-pointer pt-[7px] pr-3 pb-1.5 pl-1.5"
                htmlFor={toggleId(preset)}
              >
                <Signal id={signalId(preset)} preset={preset} signal={signals[preset]} />
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

/**
 * The filter by words (R2's `?q=`, `t f`): one small control while it holds no words, which
 * opens the box with the caret in it, and the box itself while it holds words or has the caret.
 * In the Outline sheet the box is drawn open. Leaving an empty box shuts it back into its
 * control. The box shows its name as its placeholder, so a reader sees what it is for before
 * typing (WCAG 3.3.2).
 */
function WordsFilter({
  filter,
  inSheet = false,
  onFilterChange,
}: {
  filter: string;
  inSheet?: boolean;
  onFilterChange: (next: string) => void;
}): ReactNode {
  const [opened, setOpened] = useState(false);
  const box = useRef<HTMLInputElement>(null);
  // The control's press opens the box with the caret in it, once the box is drawn.
  useEffect(() => {
    if (opened) box.current?.focus();
  }, [opened]);
  if (!inSheet && !opened && filter === "") {
    return (
      <button
        className={pageHeaderControl}
        {...{ [WORDS_FILTER_ATTRIBUTE]: "" }}
        onClick={() => setOpened(true)}
        type="button"
        {...pageChrome}
      >
        {CONTROL_LABELS.filter}
      </button>
    );
  }
  return (
    <input
      aria-label={CONTROL_LABELS.filter}
      ref={box}
      className={`min-w-0 rounded-[7px] border px-2 text-xs leading-5 outline-none ${focusVisibleRing} ${pageField} ${
        inSheet ? "min-h-6 w-full basis-full text-sm" : "w-40 md:py-px"
      }`}
      data-testid="issue-filter"
      {...{ [WORDS_FILTER_ATTRIBUTE]: "" }}
      onBlur={(event) => {
        if (event.currentTarget.value === "") setOpened(false);
      }}
      onChange={(event) => onFilterChange(event.target.value)}
      placeholder={CONTROL_LABELS.filter}
      type="search"
      value={filter}
    />
  );
}

/** The mark the filter by words carries, its small control and its box alike, by which `t f`
 *  finds it (`useIssueKeymap.ts`). */
export const WORDS_FILTER_ATTRIBUTE = "data-words-filter";

/** Which of the row's menus is open; the page opens at most one at a time. */
type OpenMenu = "nodes" | "presets" | "version" | undefined;

/** The widths the row is one control at: below 1280 px, from which R2 has the filters one row.
 *  1279 px, not `80rem`: the row's widths are the reader's pixels, as the header's are. */
const ONE_CONTROL_QUERY = "(max-width: 1279px)";

/**
 * What one control draws after its name: its count, then the noun the count is in, or what of
 * it asks something of the reader, in the prototype's `.sig` (12 px, the quiet grey; the failed
 * red for what failed and the decision violet for what waits for review, each with its word, so
 * the colour repeats what the word says). The count is the author's number and carries
 * `data-preset-count`, which the checks read it by.
 */
function Signal({
  id,
  preset,
  signal,
}: {
  id?: string;
  preset: Preset;
  signal: PresetSignal;
}): ReactNode {
  const { flag, unit } = signal;
  return (
    <span className={`text-xs leading-4 font-medium ${pageQuietText}`} id={id}>
      <span data-preset-count={preset} {...authorText}>
        {signal.count}
      </span>
      {unit === undefined ? null : <span {...pageChrome}> {FILTER_SIGNAL_WORDS[unit]}</span>}
      {flag === undefined ? null : (
        <span
          className={flag.word === "failing" ? pageFailedText : pageDecisionText}
          data-preset-flag={flag.word}
          {...pageChrome}
        >
          {" · "}
          <span {...authorText}>{flag.count}</span> {FILTER_SIGNAL_WORDS[flag.word]}
        </span>
      )}
    </span>
  );
}
