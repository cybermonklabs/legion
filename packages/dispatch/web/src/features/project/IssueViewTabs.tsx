import { type ReactNode, useEffect, useRef, useState } from "react";
import type { LegionBoardHeader } from "../../api/types";
import { PanelTrigger } from "../../components/PanelTrigger";
import {
  activeTabIndicatorBorder,
  activeTabIndicatorText,
  borderDefault,
  card,
  dangerText,
  surfaceBg,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  surfaceMutedStrongBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { type BuiltInView, type SavedView, VIEW_NAME_MAX, viewNameProblem } from "./issues-views";

/** Which tab is the page's: a built-in view, or a saved one by its name. */
export type ActiveView =
  | { readonly kind: "built-in"; readonly id: string }
  | { readonly kind: "saved"; readonly name: string };

/** The layouts of a view. */
export type ViewLayout = "table" | "board";

const PANEL_CLASS = `z-50 flex w-72 max-w-[calc(100vw-16px)] flex-col gap-2 rounded-lg border p-3 text-sm font-normal shadow-lg ${card}`;

const tabClass = (active: boolean) =>
  active
    ? `inline-flex min-h-11 shrink-0 items-center gap-1 border-b-2 px-1 py-2 text-[13px] whitespace-nowrap md:font-semibold md:min-h-9 ${activeTabIndicatorBorder} ${activeTabIndicatorText}`
    : `inline-flex min-h-11 shrink-0 items-center gap-1 border-b-2 border-transparent px-1 py-2 text-[13px] whitespace-nowrap md:min-h-9 ${textSecondaryOnCanvas}`;

const smallButton = `inline-flex min-h-9 shrink-0 items-center rounded-md border px-1 text-xs md:px-1.5 font-medium whitespace-nowrap md:min-h-8 ${borderDefault} ${surfaceMutedHoverBg} ${textSecondaryOnCanvas}`;

/** A tab's count: smaller than the shared pill, so seven tabs and a saved one fit one row. */
const countClass = `rounded-full px-1 py-0.5 text-[11px] font-medium tabular-nums ${surfaceMutedBg} ${textSecondaryOnCanvas}`;

/** The mark a tab whose view the reader changed shows. */
function ChangedMark(): ReactNode {
  return (
    <>
      <span aria-hidden className={`size-1.5 rounded-full bg-current ${textPrimaryOnCanvas}`} />
      <span className="sr-only">changed</span>
    </>
  );
}

/** The daemon's numbers and read state, for the (i) hint. */
function DaemonHint({
  header,
  pending,
}: {
  header: LegionBoardHeader | undefined;
  pending: boolean;
}): ReactNode {
  if (header === undefined)
    return <p>{pending ? "Reading the daemon" : "Dispatch has not read the daemon yet"}</p>;
  const { queue_length, read_at, read_error, read_ok, slot_cap, slots_in_use, trees } = header;
  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5" data-legion-header>
      <dt className={textMutedOnCanvas}>Slots in use</dt>
      <dd data-legion-slots>
        {slots_in_use} of {slot_cap ?? "no cap"}
      </dd>
      <dt className={textMutedOnCanvas}>Trees</dt>
      <dd data-legion-trees>{trees}</dd>
      <dt className={textMutedOnCanvas}>Waiting line</dt>
      <dd data-legion-queue-length>{queue_length}</dd>
      <dt className={textMutedOnCanvas}>Last read</dt>
      <dd data-legion-read-at>
        {read_at === null ? "Not read yet" : new Date(read_at).toLocaleString()}
        {read_ok || read_at === null ? "" : ", failed"}
      </dd>
      {read_error === null ? null : (
        <>
          <dt className={textMutedOnCanvas}>Error</dt>
          <dd className={`break-words ${dangerText}`}>{read_error}</dd>
        </>
      )}
    </dl>
  );
}

/** The Save view panel: a name of at most 14 characters, unique in the project and none of the
 *  built-in views' names. A name in use asks to replace that view. */
function SaveView({
  initial,
  onOpenChange,
  onSave,
  open,
  saved,
}: {
  initial: string;
  onOpenChange: (open: boolean) => void;
  onSave: (name: string) => void;
  open: boolean;
  saved: readonly SavedView[];
}): ReactNode {
  const [name, setName] = useState(initial);
  const [tried, setTried] = useState(false);
  const problem = viewNameProblem(name);
  const taken = saved.some((view) => view.name === name.trim());
  return (
    <PanelTrigger
      align="right"
      label="Save view"
      onOpenChange={(next) => {
        if (next) {
          setName(initial);
          setTried(false);
        }
        onOpenChange(next);
      }}
      open={open}
      panelClassName={PANEL_CLASS}
      size="compact"
      title="Save view"
      triggerClassName={smallButton}
    >
      {(close) => (
        <form
          className="flex flex-col gap-2"
          onSubmit={(event) => {
            event.preventDefault();
            setTried(true);
            if (problem !== undefined || taken) return;
            onSave(name.trim());
            close();
          }}
        >
          <label className={`flex flex-col gap-1 text-xs ${textSecondaryOnCanvas}`}>
            Name
            <input
              // biome-ignore lint/a11y/noAutofocus: the panel opens to type a name
              autoFocus
              className={`rounded-md border px-2 py-1 text-sm ${borderDefault} ${surfaceBg}`}
              maxLength={VIEW_NAME_MAX + 6}
              onChange={(event) => setName(event.target.value)}
              value={name}
            />
          </label>
          {tried && problem !== undefined ? (
            <p className={`text-xs ${dangerText}`} role="alert">
              {problem}
            </p>
          ) : null}
          {tried && problem === undefined && taken ? (
            <p className={`text-xs ${textSecondaryOnCanvas}`} role="alert">
              A saved view has this name
            </p>
          ) : null}
          <span className="flex gap-2">
            {tried && problem === undefined && taken ? (
              <button
                className={smallButton}
                onClick={() => {
                  onSave(name.trim());
                  close();
                }}
                type="button"
              >
                Replace it
              </button>
            ) : (
              <button className={smallButton} type="submit">
                Save
              </button>
            )}
            <button className={smallButton} onClick={close} type="button">
              Cancel
            </button>
          </span>
        </form>
      )}
    </PanelTrigger>
  );
}

/** A failed read of what tells the page whether the project has a Legion daemon
 *  (`GET /api/v1/settings/legion`): its error, and the way to read it again. */
export interface SettingsFailure {
  readonly message: string;
  readonly retry: () => void;
  readonly retrying: boolean;
}

/**
 * The row above the table: a tab for each view with its count (Slots against the cap), the
 * saved views after the built-in ones, the (i) hint with the daemon's numbers and read state,
 * and, in space the row keeps for them, **Reset view** and **Save view** while the view is
 * changed, so a change of the view moves no table row. Under the row, a failed read of the
 * daemon, or of the Legion setting, is an alert with its whole error.
 */
export function IssueViewTabs({
  active,
  boardHeader,
  boardPending,
  changed,
  counts,
  hasLegion,
  onDelete,
  onReset,
  onSave,
  onSelect,
  onSelectSaved,
  saved,
  savedCounts,
  settingsFailure,
  views,
}: {
  active: ActiveView;
  boardHeader: LegionBoardHeader | undefined;
  boardPending: boolean;
  changed: boolean;
  /** Each built-in view's count, by id; a view whose rows the page does not hold has none. */
  counts: ReadonlyMap<string, number | undefined>;
  hasLegion: boolean;
  onDelete: (name: string) => void;
  onReset: () => void;
  onSave: (name: string) => void;
  onSelect: (id: string) => void;
  onSelectSaved: (name: string) => void;
  saved: readonly SavedView[];
  savedCounts: ReadonlyMap<string, number | undefined>;
  /** The Legion setting's read failed; the page draws the project as one with no daemon. */
  settingsFailure: SettingsFailure | undefined;
  views: readonly BuiltInView[];
}): ReactNode {
  const [panel, setPanel] = useState<"hint" | "save" | undefined>(undefined);
  // On a phone the tab row wraps, so every tab is in view. From `md` up it is one line, which
  // scrolls sideways only if the tabs outgrow it: there the active tab is brought into it, the
  // row alone scrolling, never the page.
  const listRef = useRef<HTMLDivElement>(null);
  const countsText = [...counts.values()].join(",");
  const activeTab =
    active.kind === "built-in"
      ? `[data-view-tab="${active.id}"]`
      : `[data-saved-view-tab="${CSS.escape(active.name)}"]`;
  // The counts arrive after the tabs and widen them, so the row is scrolled again then.
  // biome-ignore lint/correctness/useExhaustiveDependencies: `countsText` is the trigger, not a read
  useEffect(() => {
    const list = listRef.current;
    const node = list?.querySelector<HTMLElement>(activeTab);
    if (list === null || list === undefined || node === null || node === undefined) return;
    const listBox = list.getBoundingClientRect();
    const box = node.getBoundingClientRect();
    if (box.left < listBox.left) list.scrollLeft += box.left - listBox.left;
    else if (box.right > listBox.right) list.scrollLeft += box.right - listBox.right;
  }, [activeTab, countsText]);
  const activeView =
    active.kind === "built-in" ? views.find((view) => view.id === active.id) : undefined;
  const readFailed = hasLegion && boardHeader !== undefined && !boardHeader.read_ok;
  return (
    // On a phone the tabs wrap onto as many lines as they need, the (i) and the space kept for
    // Reset view and Save view following the last tab in the same flow (the tablist and the
    // group beside it are `contents` there), so no tab is cut at an edge. From `md` up the row
    // is one line whose tabs scroll sideways if they ever outgrow it.
    <>
      <div
        className="flex flex-wrap items-center gap-x-1 border-b md:flex-nowrap md:gap-x-2"
        data-view-tabs
      >
        <div
          aria-label="Views"
          className={`contents md:relative md:flex md:min-w-0 md:flex-1 md:gap-0 md:overflow-x-auto md:[scrollbar-width:thin] ${borderDefault}`}
          ref={listRef}
          role="tablist"
        >
          {views.map((view) => {
            const on = active.kind === "built-in" && active.id === view.id;
            const count = counts.get(view.id);
            const shown =
              count === undefined
                ? undefined
                : view.id === "slots" && boardHeader?.slot_cap != null
                  ? `${count}/${boardHeader.slot_cap}`
                  : String(count);
            return (
              <button
                aria-label={shown === undefined ? view.name : `${view.name} ${shown}`}
                aria-selected={on}
                className={tabClass(on)}
                data-view-tab={view.id}
                key={view.id}
                onClick={() => onSelect(view.id)}
                role="tab"
                type="button"
              >
                {view.name}
                {shown === undefined ? null : <span className={countClass}>{shown}</span>}
                {on && changed ? <ChangedMark /> : null}
              </button>
            );
          })}
          {saved.map((view) => {
            const on = active.kind === "saved" && active.name === view.name;
            const count = savedCounts.get(view.name);
            return (
              <span className="inline-flex shrink-0 items-center" key={view.name}>
                <button
                  aria-label={count === undefined ? view.name : `${view.name} ${count}`}
                  aria-selected={on}
                  className={tabClass(on)}
                  data-saved-view-tab={view.name}
                  onClick={() => onSelectSaved(view.name)}
                  role="tab"
                  type="button"
                >
                  {view.name}
                  {count === undefined ? null : <span className={countClass}>{count}</span>}
                  {on && changed ? <ChangedMark /> : null}
                </button>
                {on ? (
                  <button
                    aria-label={`Delete saved view ${view.name}`}
                    className={`min-h-9 rounded px-1 text-xs ${textMutedOnCanvas} ${surfaceMutedHoverBg}`}
                    onClick={() => onDelete(view.name)}
                    title="Delete this saved view"
                    type="button"
                  >
                    ✕
                  </button>
                ) : null}
              </span>
            );
          })}
        </div>
        <div className="contents md:flex md:shrink-0 md:items-center md:gap-1.5">
          <PanelTrigger
            align="left"
            label="i"
            onOpenChange={(open) => setPanel(open ? "hint" : undefined)}
            open={panel === "hint"}
            panelClassName={PANEL_CLASS}
            title="About this view"
            triggerClassName={`inline-flex size-9 shrink-0 items-center justify-center rounded-full border text-xs font-semibold italic md:size-7 ${borderDefault} ${surfaceMutedHoverBg} ${textSecondaryOnCanvas}`}
          >
            {() => (
              <div className={`flex flex-col gap-2 text-xs ${textSecondaryOnCanvas}`}>
                <p>
                  {activeView === undefined ? "A view you saved in this browser" : activeView.hint}
                </p>
                {hasLegion ? <DaemonHint header={boardHeader} pending={boardPending} /> : null}
              </div>
            )}
          </PanelTrigger>
          <span
            aria-hidden={!changed}
            className={`flex shrink-0 items-center gap-1 md:gap-1.5 ${changed ? "" : "invisible"}`}
            data-view-changed={changed || undefined}
          >
            <button className={smallButton} disabled={!changed} onClick={onReset} type="button">
              Reset view
            </button>
            <SaveView
              initial={active.kind === "saved" ? active.name : ""}
              onOpenChange={(open) => setPanel(open ? "save" : undefined)}
              onSave={onSave}
              open={panel === "save"}
              saved={saved}
            />
          </span>
        </div>
      </div>
      {/* A failed read takes a line of its own under the row, its whole error wrapping as it
        needs: beside seven tabs at 1440 px there is no room for it, and in that room it cut
        the last tabs. */}
      {readFailed || settingsFailure !== undefined ? (
        <div className={`flex flex-col gap-0.5 text-xs ${dangerText}`}>
          {settingsFailure === undefined ? null : (
            <p className="break-words" data-legion-settings-error role="alert">
              Legion setting read failed: {settingsFailure.message}{" "}
              <button
                className="font-semibold underline disabled:opacity-50"
                disabled={settingsFailure.retrying}
                onClick={settingsFailure.retry}
                type="button"
              >
                {settingsFailure.retrying ? "Retrying…" : "Retry"}
              </button>
            </p>
          )}
          {readFailed ? (
            <p className="break-words" data-legion-read-error role="alert">
              Daemon read failed{boardHeader?.read_error ? `: ${boardHeader.read_error}` : ""}
            </p>
          ) : null}
        </div>
      ) : null}
    </>
  );
}

/** The layout control: the table or the Board, of the same view. */
export function LayoutSwitch({
  layout,
  onLayout,
}: {
  layout: ViewLayout;
  onLayout: (layout: ViewLayout) => void;
}): ReactNode {
  return (
    <fieldset
      className={`inline-flex shrink-0 rounded-lg border p-0.5 ${borderDefault} ${surfaceMutedBg}`}
      data-layout-switch
    >
      <legend className="sr-only">Layout</legend>
      {(["table", "board"] as const).map((option) => (
        <button
          aria-pressed={layout === option}
          className={`min-h-9 rounded-md px-2 text-xs font-medium md:min-h-7 ${
            layout === option ? surfaceMutedStrongBg : ""
          } ${textSecondaryOnCanvas}`}
          key={option}
          onClick={() => onLayout(option)}
          type="button"
        >
          {option === "table" ? "Table" : "Board"}
        </button>
      ))}
    </fieldset>
  );
}
