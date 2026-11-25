import { MAX_ISSUE_ADVANCES } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { apiErrorMessage, issuePatchRequest } from "../../api/client";
import { prioritiesQuery } from "../../api/queries";
import { useReadBound } from "../../api/read-bound";
import type { Issue, PriorityRow } from "../../api/types";
import { heldPress } from "../../components/held-press";
import { type ListMarks, MultiSelect } from "../../components/MultiSelect";
import { usePageWords, useTooltips } from "../../components/tooltips";
import { inputMethodKey } from "../../lib/input-method";
import {
  dangerText,
  dismissButtonText,
  focusBorder,
  inputClasses,
  linkHoverText,
  linkText,
  pageFailedText,
  pageHairBorder,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pagePaneBg,
  pagePaperBg,
  pageQuietText,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonText,
  surfaceMutedStrongBg,
  textMutedHoverToSecondary,
  textMutedOnSurface,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { propertyControl } from "../issue/property-control";
import { focusOnDocument } from "../shell/roving";
import { stoppedWriteNote } from "../writes/keyed-write";
import { endedStopped, useKeyedWrite } from "../writes/useKeyedWrite";
import { orderLabel } from "./priorities-model";
import { priorityPath } from "./priority-routes";

/** What the line calls each priority the read answered, by its id: its order and its title, as
 *  the priorities page names it ("P7 Rocq and Lean speed work restarted"). The read answers the
 *  priorities in rank order, so the order is the row's place in that one list, named by the one
 *  rule the table names it by (`orderLabel`). A priority this map does not hold is named by its
 *  own id, so a line never waits on a second read to say which priority it means. */
function priorityLabels(rows: readonly PriorityRow[]): ReadonlyMap<string, string> {
  return new Map(rows.map((row, index) => [row.id, `${orderLabel(index)} ${row.title}`]));
}

/** The name of the control that places `issueKey` under a priority, in the words it has on
 *  every page. */
export function addPriorityControlName(issueKey: string): string {
  return `Add a priority to ${issueKey}`;
}

/** The mark the add form's Priority to advance list writes on its trigger, which the form finds
 *  it by to hand it the focus as it opens. */
const PICKER_ATTRIBUTE = "data-priority-picker";
const PICKER_MARKS: ListMarks = { trigger: { [PICKER_ATTRIBUTE]: "" } };

/** The cross a priority's Remove draws inside `NoTooltips`, one em square so it takes its
 *  button's text size, in its button's text colour. It is a shape and not text because the page's
 *  typeface holds only the characters its subset lists, and ✕ is not among them: as text, ✕ is
 *  drawn in a face of the reader's own machine, at that face's width. Drawn as a shape, it is the
 *  same on every machine. The button's name says what it does, so the cross is hidden from
 *  assistive technology. */
function RemoveCross(): ReactNode {
  return (
    <svg
      aria-hidden="true"
      className="inline-block size-[1em] align-middle"
      data-remove-glyph=""
      viewBox="0 0 16 16"
    >
      <path
        d="M4.5 4.5l7 7m0-7l-7 7"
        fill="none"
        stroke="currentColor"
        strokeLinecap="round"
        strokeWidth="1.75"
      />
    </svg>
  );
}

/**
 * The `Advances` item of the issue header's metadata rail (INFRA-2947, R16 and R3): each
 * priority this issue advances, named by its order and title and linking to that priority's own
 * page, with the control that adds one beside them and a remove on each.
 *
 * An issue that advances none carries no label and no empty line — only the control that places
 * it under a priority, which is how a person acts on a row of the page's first fault list. A
 * priority inherited from an issue above (R4) says so and carries no remove: that relation is
 * the ancestor's, and a relation on a child never places the issues above it.
 *
 * Every press writes the whole of the issue's own set — `PATCH /api/v1/issues/{key}` — with the
 * `advances_revision` of the read the line drew it from. The server reads the act from the
 * difference between the set and what it holds now, so without the revision a set another writer
 * moved in between would read as this press's act and retract a relation the person never saw;
 * with it, that write is refused 409 ADVANCES_REVISION (`api/issue_advances.go`,
 * `refuseAdvancesRevision`), the line shows the server's reason as it shows every refusal, and
 * the issue is read again so the next press carries the revision it holds now. The list of
 * priorities to choose from is read only once the line has an entry to name or the control is
 * open, as the components line reads the architecture tree only while its picker is open.
 *
 * The control adds in two steps. `+` opens a small form in its own place: its Priority to
 * advance is the shared list's single pick (`MultiSelect`), whose text box narrows the priorities
 * as the reader types, and a pick stages the priority there; Add writes it, and Cancel drops it.
 * The press takes the focus away with the `+`, and the form lands it on its first control, the
 * list's trigger, with no scroll, since the form stands where the `+` stood. The list says the
 * priorities are loading while their read is out, and that they could not be loaded where it
 * failed.
 *
 * Inside `NoTooltips` (`components/tooltips.tsx`) it says the new issue page's words with their
 * marks and carries no `title`; there `inRow` draws it as the value of a row of Details, which
 * draws the label: no `Advances` of its own, a line that wraps and cuts no word, and "Add a
 * priority" on its add control while the issue advances none.
 */
export function IssuePriorityLine({
  disabled,
  inRow = false,
  issue,
}: {
  disabled: boolean;
  /** Whether the line is the value of a row of the new issue page's Details. It acts inside
   *  `NoTooltips` alone. */
  inRow?: boolean;
  issue: Pick<Issue, "advances" | "advances_revision" | "key">;
}): ReactNode {
  const tooltips = useTooltips();
  const page = usePageWords();
  const inDetails = page !== null && inRow;
  const advances = issue.advances ?? [];
  const own = advances.filter((entry) => !entry.inherited).map((entry) => entry.id);
  const [adding, setAdding] = useState(false);
  const [picked, setPicked] = useState("");
  const [listOpen, setListOpen] = useState(false);
  const queryClient = useQueryClient();
  const priorities = useQuery({
    ...prioritiesQuery(),
    enabled: adding || advances.length > 0,
  });
  const rows = useMemo(() => priorities.data?.priorities ?? [], [priorities.data]);
  const labels = useMemo(() => priorityLabels(rows), [rows]);
  // Why the reads a write made stale have not settled, once the hold over them ran out.
  const [unsettled, holdThrough] = useReadBound();
  const write = useKeyedWrite({
    // The line says why a write was refused, beside the control it came from.
    inline: true,
    label: () => `Advances of ${issue.key}`,
    // One write to the issue, in press order with every other (`issue:<KEY>`).
    target: () => `issue:${issue.key}`,
    readsLate: true,
    // The set and the revision of the read it was drawn from, both taken at the press: a retry
    // sends the same pair, never a revision read after the set was chosen.
    request: (input: { advances: string[]; advances_revision: number }) =>
      issuePatchRequest(issue.key, input),
    onSettled: () => {
      // Every open issue, this one included: a descendant inherits the change with no event of
      // its own, and a refused write leaves this issue's read stale, so the next press must carry
      // the revision it holds now. The priorities body and the rail's two counts move with the
      // relation as well.
      const rereads = [
        queryClient.invalidateQueries({ queryKey: ["issue"] }),
        queryClient.invalidateQueries({ queryKey: ["priorities"] }),
      ];
      // Outside `NoTooltips` the write stops pending as it lands and these reads go on after it,
      // as on `main`. Inside, `onSettled` returns them, which TanStack Query awaits before the
      // write stops pending: each control of the line stays refused, and keeps the focus, until
      // the issue it draws is the one read after the write, or until the request deadline where
      // that read has not settled - one the network paused while the browser is offline - so no
      // read holds the line for good, and the line says why until the reads settle
      // (`useReadBound`).
      if (page === null) return;
      return holdThrough(Promise.all(rereads));
    },
  });
  const writeOwn = (ids: string[]) =>
    write.mutate({ advances: ids, advances_revision: issue.advances_revision });
  // Disabled on a closed issue; held while a write is out within its deadline, so each control
  // keeps the focus the press gave it and sends nothing more until the write lands (`heldPress`):
  // a priority's Remove, the form's Add and the add control, on both sides of `NoTooltips`.
  // Inside, the write is out until the issue is read again (`onSettled`, above). A write past its
  // deadline (`late`) holds them no longer: it is still tried under its key, and the shell's
  // indicator offers to stop it, but one that never answers - the browser offline - would
  // otherwise hold the line for good. A press made then waits its turn behind it on the issue's
  // line (`target`), late as well.
  const held = write.isPending && !write.late;
  const addControl = useRef<HTMLButtonElement>(null);
  const form = useRef<HTMLFormElement>(null);
  // Opening the form puts the focus in its first field, the list's trigger: the add control the
  // form replaces leaves the page, and without this the focus would drop to the body with it and
  // a keyboard user would have to find the form again. It scrolls nothing, since the form stands
  // where the add control stood.
  useEffect(() => {
    if (!adding) return;
    form.current
      ?.querySelector<HTMLElement>(`[${PICKER_ATTRIBUTE}]`)
      ?.focus({ preventScroll: true });
  }, [adding]);
  // Closing it - Add, Cancel or Escape - owes the add control, drawn again in its place, the
  // focus the form held, for the same reason. The control is held, never disabled, while a write
  // is out, so it takes the focus at once. Only a focus still on the document is taken back
  // (`focusOnDocument`): anywhere else, the reader put it there in between.
  const focusAddControl = useRef(false);
  useEffect(() => {
    if (adding || !focusAddControl.current) return;
    focusAddControl.current = false;
    if (focusOnDocument()) addControl.current?.focus();
  }, [adding]);
  const closeForm = () => {
    setPicked("");
    setAdding(false);
    focusAddControl.current = true;
  };
  // Inside `NoTooltips` a press of a priority's Remove is remembered, with the issue's own entries
  // drawn after it and then before it, the nearest first, until its write stops pending, which is
  // once the issue is read again (`onSettled` returns its reads); each Remove is kept by its
  // entry's id. A Remove is never `disabled` while its write is out, so while its entry is drawn -
  // the write failed, or the read kept it - it keeps the focus and nothing moves it. Once the read
  // has taken the entry off the page, a focus that fell with it to the document
  // (`focusOnDocument`) goes to the Remove of the nearest of those entries still drawn, else to
  // the add control; a focus anywhere else is left where it is. The press is forgotten whenever
  // the write stops pending. It is state, not a ref, so the render the press makes reads the
  // write pending, and a write that ends before its own pending render is drawn still ends the
  // press.
  const [removePressed, setRemovePressed] = useState<{
    id: string;
    nearest: readonly string[];
  } | null>(null);
  const removeButtons = useRef(new Map<string, HTMLButtonElement>());
  useEffect(() => {
    if (removePressed === null || write.isPending) return;
    setRemovePressed(null);
    const buttons = removeButtons.current;
    if (buttons.has(removePressed.id) || !focusOnDocument()) return;
    const target =
      removePressed.nearest.map((id) => buttons.get(id)).find((button) => button !== undefined) ??
      addControl.current;
    target?.focus();
  }, [removePressed, write.isPending]);
  // Inside `NoTooltips` each colour is one of the new issue page's: the line is the ink in a row
  // of Details (the prototype's `.frow .fv`) and the muted grey elsewhere (its `#hdr-props`); a
  // priority is a link in the ink; what qualifies it is quiet words, a refusal the failed red;
  // the controls are muted words that turn ink under the pointer, the add control on the page's
  // pane, Add with the header control's rule, and the picker the page's field, which keeps
  // `main`'s focus rule, the application's one focus indicator.
  const quiet = page === null ? textMutedOnSurface : pageQuietText;
  const failed = page === null ? dangerText : pageFailedText;
  // A failed save shows the server's reason, else the page's own line. A write the reader
  // stopped may have been saved, and the line offers no Retry of it: each press here is a new
  // write of the set the line reads, under a key of its own. Each carries the mark of whose
  // words it is: the stopped note and the page's own line are the page's, a server's reason is
  // not.
  const notSaved =
    page?.ANSWER_LINES.priorities_not_saved ?? "Could not save what this issue advances.";
  const stopped = endedStopped(write);
  const saveFailure = stopped
    ? stoppedWriteNote("what this issue advances", "saved", {
        anew: "It is read again, and adding or removing a priority sends a new write.",
      })
    : apiErrorMessage(write.error, notSaved);
  const failureIsPageWords = stopped || saveFailure === notSaved;
  // What the line says when the priorities could not be read: below the picker and on the line,
  // and on the line before why the reads a write made stale have not settled (`unsettled`).
  const prioritiesUnreadable =
    page?.ANSWER_LINES.priorities_unreadable ?? "Could not load the priorities.";
  // A priority the issue already advances is no choice: the line names it, whether the relation
  // is the issue's own or an ancestor's.
  const choices = rows.filter((row) => !advances.some((entry) => entry.id === row.id));
  const atCap = own.length >= MAX_ISSUE_ADVANCES;
  // Inside `NoTooltips` nothing of the line keeps its width, in a row of Details and in the
  // header alike: a priority's title and a refusal's reason are values, which break anywhere and
  // stay within their line - the title with `min-w-0`, a flex item that shrinks beside the mark
  // after it, the reason with `max-w-full` - rather than push the line past its row. Outside,
  // each part stays whole, as on `main`.
  const wraps = page !== null;

  const add = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (picked === "" || held) return;
    writeOwn([...own, picked]);
    closeForm();
  };

  return (
    <div
      className={`flex ${wraps ? "min-w-0 flex-wrap" : "shrink-0"} items-center gap-2 text-sm ${page === null ? textSecondaryOnSurface : inDetails ? pageInkText : pageMutedText}`}
      data-testid={inDetails ? undefined : "issue-advances"}
    >
      {advances.length === 0 || inDetails ? null : (
        <span className="font-medium" {...page?.pageChrome}>
          {page?.PROPERTY_LABELS.advances ?? "Advances"}
        </span>
      )}
      {advances.map((entry) => {
        const label = labels.get(entry.id) ?? entry.id;
        return (
          <span
            className={`flex ${wraps ? "min-w-0" : "shrink-0"} items-center gap-1`}
            key={entry.id}
          >
            {/* Inside `NoTooltips` the priority's title wraps inside its line rather than
                widening it; its words are the person's who wrote it. */}
            <Link
              className={`dispatch-inline-link ${wraps ? "min-w-0 [overflow-wrap:anywhere]" : "shrink-0 whitespace-nowrap"} underline ${page === null ? `${linkText} ${linkHoverText}` : pageInkText}`}
              title={tooltips ? label : undefined}
              to={priorityPath(entry.id)}
              {...page?.authorText}
            >
              {label}
            </Link>
            {entry.inherited ? (
              <span className={`shrink-0 text-xs ${quiet}`} {...page?.pageChrome}>
                {page?.CONTROL_LABELS.advances_inherited ?? "inherited"}
              </span>
            ) : (
              <button
                aria-label={`Remove ${label} from ${issue.key}`}
                className={`shrink-0 refused:cursor-not-allowed refused:opacity-50 ${page === null ? dismissButtonText : `${pageMutedText} hover:text-page-ink`}`}
                disabled={disabled}
                {...heldPress(held, () => {
                  if (page !== null) {
                    const at = own.indexOf(entry.id);
                    setRemovePressed({
                      id: entry.id,
                      nearest: [...own.slice(at + 1), ...own.slice(0, at).reverse()],
                    });
                  }
                  writeOwn(own.filter((id) => id !== entry.id));
                })}
                ref={(button) => {
                  if (button === null) removeButtons.current.delete(entry.id);
                  else removeButtons.current.set(entry.id, button);
                }}
                type="button"
                {...page?.pageChrome}
              >
                {page === null ? "✕" : <RemoveCross />}
              </button>
            )}
          </span>
        );
      })}
      {atCap ? (
        <span className={`${wraps ? "" : "shrink-0 "}text-xs ${quiet}`} {...page?.pageChrome}>
          {page === null ? (
            <>An issue advances at most {MAX_ISSUE_ADVANCES} priorities.</>
          ) : (
            page.STATE_LINES.advances_at_cap
          )}
        </span>
      ) : adding ? (
        <form
          className={`flex ${wraps ? "min-w-0 flex-wrap" : "shrink-0"} items-center gap-2`}
          onKeyDown={(event) => {
            // A press that ends an IME composition gives up what the reader was composing, not
            // the form: the form leaves it alone, unmarked, as the key registry does. Safari
            // sends it with `isComposing` false and keyCode 229 (`inputMethodKey`).
            if (event.key !== "Escape" || inputMethodKey(event.nativeEvent)) return;
            // Escape closes this form and goes no further. The panel the line sits in closes on
            // Escape too - Details on the new issue page, which on a phone is a sheet whose
            // Escape is the key registry's dialog binding (`useDialog`) - and one press reaching
            // it would close the form and the panel together and leave the focus on nothing. The
            // registry passes over a press already `defaultPrevented` (`keymap.ts`), and a press
            // stopped here reaches no listener above the form either. A second Escape, pressed on
            // the add control the focus returns to, is the panel's. An Escape in the open list is
            // the list's own: its panel takes it before it reaches the form (`AnchoredPanel`).
            event.preventDefault();
            event.stopPropagation();
            closeForm();
          }}
          onSubmit={add}
          ref={form}
          // Inside `NoTooltips` the form's words are the page's - the list's name, what its
          // trigger says while nothing is picked, Add and Cancel - and carry its mark; a picked
          // priority's title carries the author's.
          {...page?.pageChrome}
        >
          <MultiSelect
            emptyMessage={
              priorities.isPending
                ? (page?.ANSWER_LINES.loading_priorities ?? "Loading priorities…")
                : priorities.isError || choices.length > 0
                  ? "No priority matches that."
                  : "No priority left to add."
            }
            error={priorities.isError ? prioritiesUnreadable : null}
            label={page?.CONTROL_LABELS.priority_to_advance ?? "Priority to advance"}
            marks={PICKER_MARKS}
            mode="single"
            onOpenChange={setListOpen}
            onPick={setPicked}
            open={listOpen}
            optionLabel={(id) => labels.get(id)}
            options={choices.map((row) => row.id)}
            searchLabel="Search priorities"
            // A fixed width, the 224 px the native select this list replaced was capped at: as
            // wide as that select was once any option reached the cap, so the form beside it
            // never moves as the reader picks, nor once the priorities have loaded, and a title
            // longer than that ends in an ellipsis, as it did in the select. Inside `NoTooltips`
            // it is the page's field, and a long title wraps (`MultiSelect`).
            triggerClassName={`min-h-11 w-56 shrink-0 rounded-lg px-2 py-1 text-left text-sm md:min-h-8 ${page === null ? inputClasses(true) : `${pageHairBorder} ${pagePaperBg} ${pageInkText} ${focusBorder}`}`}
            value={picked === "" ? undefined : picked}
          >
            {picked === "" ? (
              priorities.data === undefined ? (
                (page?.ANSWER_LINES.loading_priorities ?? "Loading priorities…")
              ) : (
                (page?.CONTROL_LABELS.choose_priority ?? "Choose a priority")
              )
            ) : page === null ? undefined : (
              <span {...page.authorText}>{labels.get(picked) ?? picked}</span>
            )}
          </MultiSelect>
          <button
            // While no priority is picked it is `disabled` alone, on both sides of `NoTooltips`.
            aria-disabled={picked === "" ? undefined : held || undefined}
            className={`min-h-11 rounded px-2 py-1 text-sm font-medium refused:cursor-not-allowed md:min-h-8 ${page === null ? `${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonDisabledText}` : `${pageLineBorder} ${pageMutedText} refused:text-page-quiet`}`}
            disabled={disabled || picked === ""}
            type="submit"
            {...page?.pageChrome}
          >
            {page?.CONTROL_LABELS.add ?? "Add"}
          </button>
          <button
            className={`min-h-11 rounded px-2 py-1 text-sm font-medium md:min-h-8 ${page === null ? textMutedHoverToSecondary : `${pageQuietText} hover:text-page-muted`}`}
            onClick={closeForm}
            type="button"
            {...page?.pageChrome}
          >
            {page?.CONTROL_LABELS.cancel ?? "Cancel"}
          </button>
        </form>
      ) : (
        <button
          // One character and its name, as the remove control beside it is: an issue that
          // advances nothing still carries this control, and a word here puts the issue
          // header's metadata rail over its width on a desktop, which would leave every bare
          // issue's rail scrolling and faded (e2e/issue.e2e.ts, "the details rail fades its
          // trailing edge only while it has somewhere to scroll"). A control whose whole label
          // is an icon carries no visible text for WCAG 2.5.3 to match. The character is a
          // plain "+" (U+002B): the fullwidth "＋" this line held draws as an empty box where
          // no CJK font is installed, which is this pod's Chromium and some readers' browsers.
          // In a row of Details, with no priority to stand beside, it says what it does in the
          // words its name begins with, because that panel raises no tooltip that could say it
          // instead.
          aria-label={addPriorityControlName(issue.key)}
          // Inside `NoTooltips` it is 28 px tall from `md`, as the controls beside it in Details
          // are (the repository's box, `main`'s input in `RepoControl.tsx`, with its `md:min-h-7`;
          // the labels' and the components' triggers' `sm:min-h-7`); below 1280 px `styles.css`
          // gives each of them a 44 px floor all the same. Outside it the class is this tree's.
          className={`inline-flex min-h-11 shrink-0 items-center rounded-full px-2 py-2 text-sm whitespace-nowrap outline-none refused:cursor-not-allowed refused:opacity-50 ${page === null ? `${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary}` : `${pagePaneBg} ${pageMutedText} hover:text-page-ink md:min-h-7 md:py-1`}`}
          disabled={disabled}
          {...heldPress(held, () => setAdding(true))}
          ref={addControl}
          title={tooltips ? addPriorityControlName(issue.key) : undefined}
          type="button"
          // The new issue page's add menu reaches the control by this mark while the issue
          // advances none, rather than drawing a second one, and Details' priority hand-offs find
          // it by the same mark (`property-control.ts`).
          {...propertyControl("advances", issue.key)}
          {...page?.pageChrome}
        >
          {page !== null && inRow && advances.length === 0 ? page.CONTROL_LABELS.add_priority : "+"}
        </button>
      )}
      {priorities.isError ? (
        <span
          className={`${wraps ? "" : "shrink-0 "}text-xs ${failed}`}
          role="alert"
          {...page?.pageChrome}
        >
          {prioritiesUnreadable}
        </span>
      ) : null}
      {write.error === null ? null : (
        <span
          className={`${wraps ? "max-w-full [overflow-wrap:anywhere] " : "shrink-0 "}text-xs ${failed}`}
          role="alert"
          {...(failureIsPageWords ? page?.pageChrome : page?.authorText)}
        >
          {saveFailure}
        </span>
      )}
      {/* Only inside `NoTooltips` does the line hold through the reads its write made stale, so
          only there does a read outlive its hold and say why here, until it settles. */}
      {unsettled === undefined ? null : (
        <span
          className={`${wraps ? "max-w-full [overflow-wrap:anywhere] " : "shrink-0 "}text-xs ${failed}`}
          role="alert"
          {...page?.pageChrome}
        >
          {`${prioritiesUnreadable} ${unsettled}.`}
        </span>
      )}
    </div>
  );
}
