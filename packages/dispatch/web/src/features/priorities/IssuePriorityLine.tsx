import { MAX_ISSUE_ADVANCES } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import { apiErrorMessage, issuePatchRequest } from "../../api/client";
import { prioritiesQuery } from "../../api/queries";
import type { Issue, PriorityRow } from "../../api/types";
import { MultiSelect } from "../../components/MultiSelect";
import { usePageWords } from "../../components/tooltips";
import {
  dangerText,
  focusVisibleRing,
  pageFailedText,
  pageInkText,
  pageMutedText,
  pagePaneBg,
  pageQuietText,
  secondaryButtonBorder,
  secondaryButtonGroupDisabledText,
  secondaryButtonGroupHoverBorder,
  secondaryButtonText,
} from "../../theme/classes";
import { stoppedWriteNote } from "../writes/keyed-write";
import { endedStopped, useKeyedWrite } from "../writes/useKeyedWrite";
import { PriorityChip } from "./PriorityChip";
import { priorityName } from "./priorities-model";
import { priorityPath } from "./priority-routes";

/** What the line knows of each priority the read answered, by its id: its place in the one
 *  order, from 1, and its title. The read answers the priorities in rank order, so the place is
 *  the row's place in that one list, named by the one rule the table names it by (`orderLabel`).
 *  A priority this map does not hold is named by its own id, so a line never waits on a second
 *  read to say which priority it means. */
function priorityPlaces(
  rows: readonly PriorityRow[]
): ReadonlyMap<string, { readonly place: number; readonly title: string }> {
  return new Map(rows.map((row, index) => [row.id, { place: index + 1, title: row.title }]));
}

/** The name of the control that places `issueKey` under a priority while the issue advances
 *  none, as the picker's trigger carries it in a row of the new issue page's Details: the words
 *  it draws there, the joining word and the issue's key ("Add a priority to CORE-1"). The new
 *  issue page's one add control finds it by that name. The page's words (`add_priority`,
 *  `control_name_to`) are written out here as a literal, as `main` writes them, since no control
 *  imports the page's word list (`components/tooltips.tsx`). */
export function addPriorityControlName(issueKey: string): string {
  return `Add a priority to ${issueKey}`;
}

/** The name of the picker's trigger on `issueKey`'s line: the add name (`addPriorityControlName`)
 *  where the trigger `adds` - in a row of the new issue page's Details while the issue advances
 *  none, where it draws "Add a priority" - and "Edit priorities" everywhere else, since there it
 *  adds and takes off alike, as the labels' trigger is named "Edit labels". That is the page's
 *  word `edit_priorities` (`shared-writes.test.ts` holds the two equal), written out here for the
 *  same reason. The hand-offs of Details find the trigger by this name (`usePriorityHandOffs`). */
export function priorityTriggerName(issueKey: string, adds: boolean): string {
  return adds ? addPriorityControlName(issueKey) : "Edit priorities";
}

/** The issue's own set while the picker is open, and the read it was drawn from: the set the
 *  close compares it with, and the revision the close's write carries. */
interface Draft {
  readonly ids: string[];
  readonly from: readonly string[];
  readonly revision: number;
}

/**
 * The item after the P0-P3 tag in the issue header's state row from 768 px up, and below it the
 * first item of the header's details row, where the state row has no room left (INFRA-2947, R16
 * and R3): each
 * priority this issue advances, as the chip of its place in the one order (`PriorityChip`, `#1`)
 * that links to that priority's own page, its title in its name and its tooltip, and the shared
 * `MultiSelect` that adds and removes the issue's own, as the labels and components are edited.
 * Like those it stages a draft while open and saves the final draft once, when it closes. The
 * chips are the line's only words, so it takes no more of the row than a few tags do, whatever
 * the titles: no `Advances` stands before them, and the group they make carries that word as its
 * name for a screen reader alone. That row wraps, and so does the line: a refusal's reason takes
 * a line of its own under the chips rather than push the row past the header's edge.
 *
 * An issue that advances none carries no chip and no empty label — only the picker's trigger,
 * which places it under a priority, and which is how a person acts on a row of the page's first
 * fault list. A priority inherited from an issue above (R4) is its chip's muted style, and its
 * name and tooltip say so; the picker does not offer it: that relation is the ancestor's, a
 * relation on a child never places the issues above it, and every `MultiSelect` row is a toggle,
 * so a row for it would offer a retraction this issue cannot make.
 *
 * Each close that changed the set writes the whole of the issue's own set —
 * `PATCH /api/v1/issues/{key}` — with the `advances_revision` of the read the draft was drawn
 * from when the picker opened. The server reads the act from the difference between the set and
 * what it holds now, so without the revision a set another writer moved in between would read as
 * this write's act and retract a relation the person never saw; with it, that write is refused
 * 409 ADVANCES_REVISION (`api/issue_advances.go`, `refuseAdvancesRevision`), the line shows the
 * server's reason as it shows every refusal, and the issue is read again so the next write
 * carries the revision it holds now. The list of priorities to choose from is read only once the
 * line has an entry to name or the picker is open, as the components line reads the
 * architecture tree only while its picker is open.
 *
 * Inside `NoTooltips` (`components/tooltips.tsx`) it says the new issue page's words with their
 * marks and carries no `title`: each priority is a link of its order and title, with the word
 * that it is inherited after it, and the line's own label before them all. There `inRow` draws
 * it as the value of a row of Details, which draws the label: no `Advances` of its own, a line
 * that wraps and cuts no word, and "Add a priority" on the picker's trigger while the issue
 * advances none, which is then named by `addPriorityControlName`.
 */
export function IssuePriorityLine({
  className,
  disabled,
  inRow = false,
  issue,
}: {
  /** How the row the line stands in places it, outside `NoTooltips`: the issue header's details
   *  row, which scrolls sideways on a phone, keeps it whole (`shrink-0`). */
  className?: string;
  disabled: boolean;
  /** Whether the line is the value of a row of the new issue page's Details. It acts inside
   *  `NoTooltips` alone. */
  inRow?: boolean;
  issue: Pick<Issue, "advances" | "advances_revision" | "key">;
}): ReactNode {
  const page = usePageWords();
  const inDetails = page !== null && inRow;
  const advances = issue.advances ?? [];
  const own = advances.filter((entry) => !entry.inherited).map((entry) => entry.id);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [inputError, setInputError] = useState<string | null>(null);
  const open = draft !== null;
  const queryClient = useQueryClient();
  const priorities = useQuery({
    ...prioritiesQuery(),
    enabled: open || advances.length > 0,
  });
  const rows = useMemo(() => priorities.data?.priorities ?? [], [priorities.data]);
  const places = useMemo(() => priorityPlaces(rows), [rows]);
  // A priority's order and title, as the picker offers it and the new issue page names it.
  const labelOf = (id: string) => {
    const known = places.get(id);
    return priorityName(id, known?.place, known?.title);
  };
  const write = useKeyedWrite({
    // The line says why a write was refused, beside the control it came from.
    inline: true,
    label: () => `Advances of ${issue.key}`,
    // One write to the issue, in press order with every other (`issue:<KEY>`).
    target: () => `issue:${issue.key}`,
    // The set and the revision of the read it was drawn from, both taken when the picker
    // opened: a retry sends the same pair, never a revision read after the set was chosen.
    request: (input: { advances: string[]; advances_revision: number }) =>
      issuePatchRequest(issue.key, input),
    onSettled: () => {
      // Every open issue, this one included: a descendant inherits the change with no event of
      // its own, and a refused write leaves this issue's read stale, so the next write must
      // carry the revision it holds now. The priorities body and the rail's two counts move with
      // the relation as well.
      const rereads = [
        queryClient.invalidateQueries({ queryKey: ["issue"] }),
        queryClient.invalidateQueries({ queryKey: ["priorities"] }),
      ];
      // Outside `NoTooltips` the write stops pending as it lands and these reads go on after it,
      // as on `main`. Inside, `onSettled` returns them, which TanStack Query awaits before the
      // write stops pending: the trigger stays `aria-disabled`, and refuses to open, until the
      // issue it draws is the one read after the write.
      if (page === null) return;
      return Promise.all(rereads);
    },
  });
  const saving = write.isPending;

  // What the picker offers: the live priorities in rank order, less those the issue inherits,
  // which are no choice here; and each of the issue's own, even one the read does not hold, so a
  // relation the line names can always be taken off.
  const live = rows
    .map((row) => row.id)
    .filter((id) => !advances.some((entry) => entry.inherited && entry.id === id));
  const options = [...live, ...own.filter((id) => !live.includes(id))];

  const setOpen = (next: boolean) => {
    if (next) {
      if (disabled || saving) return;
      setDraft({ from: own, ids: [...own], revision: issue.advances_revision });
      setInputError(null);
      return;
    }
    if (draft === null) return;
    setDraft(null);
    setInputError(null);
    // A close that changed nothing writes nothing; ids are unique, so equal lengths and every id
    // of the draft in the set it was drawn from is the same set in another order.
    if (
      draft.ids.length !== draft.from.length ||
      draft.ids.some((id) => !draft.from.includes(id))
    ) {
      write.mutate({ advances: draft.ids, advances_revision: draft.revision });
    }
  };
  const changeDraft = (next: string[]) => {
    if (next.length > MAX_ISSUE_ADVANCES) {
      setInputError(
        page?.STATE_LINES.advances_at_cap ??
          `An issue advances at most ${MAX_ISSUE_ADVANCES} priorities.`
      );
      return;
    }
    setDraft((current) => (current === null ? current : { ...current, ids: next }));
    setInputError(null);
  };

  // Inside `NoTooltips` each colour is one of the new issue page's: the line is the ink in a row
  // of Details (the prototype's `.frow .fv`) and the muted grey elsewhere (its `#hdr-props`); a
  // priority is a link in the ink; what qualifies it is quiet words, a refusal the failed red;
  // the trigger is muted words on the page's pane that turn ink under the pointer.
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
  // In a row of Details, with no priority to stand beside, the trigger says what it does, as the
  // labels' says "Add a label", because that panel raises no tooltip that could say it instead.
  // Its name then begins with the words it draws and ends with the joining word and the issue's
  // key ("Add a priority to CORE-1"), so a voice user says what they read (WCAG 2.5.3 Label in
  // Name). Everywhere else it draws "+" and is named "Edit priorities", as the labels' trigger is
  // named "Edit labels". `priorityTriggerName` gives both names.
  const addWords =
    page !== null && inRow && advances.length === 0 ? page.CONTROL_LABELS.add_priority : null;
  const triggerName = priorityTriggerName(issue.key, addWords !== null);

  const picker = (
    <MultiSelect
      // Not disabled while it saves what the close committed: a disabled button cannot hold
      // focus, so Escape's return to the trigger would be undone by the save's own render.
      // `setOpen` is what refuses to reopen until the save has landed, and `aria-disabled` is
      // how the reader is told it is busy without focus being taken away.
      ariaDisabled={saving}
      disabled={disabled}
      emptyMessage={
        priorities.isPending
          ? (page?.ANSWER_LINES.loading_priorities ?? "Loading priorities…")
          : "No priority to choose."
      }
      error={inputError}
      label="Priorities"
      onChange={changeDraft}
      onOpenChange={setOpen}
      open={open}
      optionLabel={labelOf}
      options={options}
      searchLabel={page?.CONTROL_LABELS.priority_to_advance ?? "Priority to advance"}
      selected={draft?.ids ?? own}
      // One character and its name, as the labels' trigger is: an issue that advances nothing
      // still carries this control, and a word here would widen the row it stands in for every
      // bare issue. A control whose whole label is an icon carries no visible text for WCAG 2.5.3
      // to match. The character is a plain "+" (U+002B): the fullwidth "＋" this line held draws
      // as an empty box where no CJK font is installed, which is this pod's Chromium and some
      // readers' browsers.
      triggerAriaLabel={triggerName}
      // Outside `NoTooltips` it is drawn as the labels' trigger draws its "+" in the details row
      // (`IssueLabels`, `variant="rail"`): a small bordered square, here as tall as the chips
      // beside it, inside a hit area that draws nothing: 44 px on a phone, and 32 px from `md`,
      // as the P0-P3 tag's beside it is (`badge-select.ts`). Inside `NoTooltips` it is 28 px tall
      // from `md`, as the controls beside it in Details are (the repository's box, `main`'s input
      // in `RepoControl.tsx`, with its `md:min-h-7`; the labels' and the components' triggers'
      // `sm:min-h-7`). Below 1280 px `styles.css` gives each a 44 px floor. On a phone, after a
      // chip, the square stands 8 px after the chip's edge (`PriorityChip` gives its link no room
      // at its right there): the hit area starts at that edge, over the line's gap, and keeps its
      // 44 px to the square's right, above the chip's own target where the two meet. The square
      // lights under the pointer and dims while the trigger is `disabled` or `aria-disabled`, as
      // the labels' trigger does: the trigger is the `group` its states are read from, since the
      // square itself is never disabled.
      triggerClassName={
        page === null
          ? `group inline-flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded-lg outline-none focus-visible:ring-2 max-md:relative max-md:[a+&]:-ml-3 max-md:[a+&]:justify-start max-md:[a+&]:pl-2 md:min-h-8 md:min-w-8 ${focusVisibleRing}`
          : `inline-flex min-h-11 shrink-0 items-center rounded-full px-2 py-2 text-sm whitespace-nowrap outline-none ${pagePaneBg} ${pageMutedText} hover:text-page-ink md:min-h-7 md:py-1`
      }
    >
      {page === null ? (
        <span
          className={`inline-flex size-5.5 items-center justify-center rounded-lg border text-xs leading-none font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonGroupHoverBorder} ${secondaryButtonGroupDisabledText}`}
        >
          +
        </span>
      ) : (
        <span {...page.pageChrome}>{addWords ?? "+"}</span>
      )}
    </MultiSelect>
  );
  // Nothing of the line keeps its width: a refusal's reason is a value, which breaks anywhere and
  // stays within its line (`max-w-full`) rather than push the line past its row, whether that row
  // is a row of Details inside `NoTooltips` or the issue header's state row, which wraps.
  const refusals = (
    <>
      {priorities.isError ? (
        <span
          className={`${page === null ? "max-w-full [overflow-wrap:anywhere] " : ""}text-xs ${failed}`}
          role="alert"
          {...page?.pageChrome}
        >
          {page?.ANSWER_LINES.priorities_unreadable ?? "Could not load the priorities."}
        </span>
      ) : null}
      {write.error === null ? null : (
        <span
          className={`max-w-full [overflow-wrap:anywhere] text-xs ${failed}`}
          role="alert"
          {...(failureIsPageWords ? page?.pageChrome : page?.authorText)}
        >
          {saveFailure}
        </span>
      )}
    </>
  );

  if (page === null)
    return (
      // biome-ignore lint/a11y/useSemanticElements: the chips are links and a picker's trigger, which no form's fieldset holds; the group names them for a screen reader, since no word on the row does.
      <div
        aria-label="Advances"
        className={`flex max-w-full flex-wrap items-center gap-1 max-md:gap-x-3${className === undefined ? "" : ` ${className}`}`}
        data-testid="issue-advances"
        role="group"
      >
        {advances.map((entry) => {
          const known = places.get(entry.id);
          return (
            <PriorityChip
              id={entry.id}
              inherited={entry.inherited}
              key={entry.id}
              place={known?.place}
              title={known?.title}
            />
          );
        })}
        {picker}
        {refusals}
      </div>
    );
  return (
    <div
      className={`flex min-w-0 flex-wrap items-center gap-2 text-sm ${inDetails ? pageInkText : pageMutedText}`}
      data-testid={inDetails ? undefined : "issue-advances"}
    >
      {advances.length === 0 || inDetails ? null : (
        <span className="font-medium" {...page.pageChrome}>
          {page.PROPERTY_LABELS.advances}
        </span>
      )}
      {advances.map((entry) => (
        <span className="flex min-w-0 items-center gap-1" key={entry.id}>
          {/* The priority's title wraps inside its line rather than widening it; its words are
              the person's who wrote it. */}
          <Link
            className={`dispatch-inline-link min-w-0 [overflow-wrap:anywhere] underline ${pageInkText}`}
            to={priorityPath(entry.id)}
            {...page.authorText}
          >
            {labelOf(entry.id)}
          </Link>
          {entry.inherited ? (
            <span className={`shrink-0 text-xs ${pageQuietText}`} {...page.pageChrome}>
              {page.CONTROL_LABELS.advances_inherited}
            </span>
          ) : null}
        </span>
      ))}
      {picker}
      {refusals}
    </div>
  );
}
