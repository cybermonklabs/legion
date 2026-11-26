import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useCallback, useMemo, useState } from "react";

import { projectIssuesQuery } from "../../api/queries";
import { CappedGroup } from "../../components/CappedGroup";
import { MultiSelect } from "../../components/MultiSelect";
import { type PageWords, usePageWords, withoutEllipsis } from "../../components/tooltips";
import { pageLabelChip, pagePaperBg, surfaceMutedStrongBg } from "../../theme/classes";
import {
  pageLineBorder,
  pageMutedText,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textSecondaryOnSurface,
} from "../../theme/shell-classes";

const maxLabelLength = 40;
const maxLabels = 20;

function labelKey(label: string): string {
  return label.toLowerCase();
}

function uniqueLabels(labels: readonly string[]): string[] {
  const result: string[] = [];
  const seen = new Set<string>();
  for (const rawLabel of labels) {
    const label = rawLabel.trim();
    if (label === "" || seen.has(labelKey(label))) continue;
    seen.add(labelKey(label));
    result.push(label);
  }
  return result;
}

function includesLabel(labels: readonly string[], label: string): boolean {
  return labels.some((current) => labelKey(current) === labelKey(label));
}

function labelsMatch(left: readonly string[], right: readonly string[]): boolean {
  const leftKeys = new Set<string>();
  const rightKeys = new Set<string>();
  for (const label of left) leftKeys.add(labelKey(label));
  for (const label of right) rightKeys.add(labelKey(label));
  if (leftKeys.size !== rightKeys.size) return false;
  for (const label of leftKeys) {
    if (!rightKeys.has(label)) return false;
  }
  return true;
}

function saveLabels(draft: readonly string[], labels: readonly string[]): string[] {
  return [
    ...labels.filter((label) => includesLabel(draft, label)),
    ...draft.filter((label) => !includesLabel(labels, label)),
  ];
}

/** `main`'s name for the labels control: the name it answers to in every state outside
 *  `NoTooltips`, and inside while the control draws a label (`displayLabels`: the open popover's
 *  draft, else the issue's labels) and no save is in flight. */
const editLabelsName = "Edit labels";

/** The name the labels control answers to, which the new issue page's add menu, its `l` key and
 *  its rows find it by inside `NoTooltips` rather than drawing a second one. `inside` is
 *  required: the control's state inside `NoTooltips`, or `null` outside it, where the name is
 *  `main`'s "Edit labels" in every state. Inside it carries the labels the control draws,
 *  whether a save is in flight, and the page's words (`ANSWER_LINES` and `CONTROL_LABELS`). With
 *  no label the name is the words the control draws, then the list's `control_name_to` and the
 *  issue's key ("Add a label to CORE-1", as the priority's is "Add a priority to CORE-1"), so a
 *  voice user saying what they read presses it (WCAG 2.5.3); with no key it is those words
 *  alone. With a label it is `main`'s "Edit labels". While a save is in flight the control draws
 *  `saving_labels` ("Saving labels…") whatever it holds, so the name is those words with their
 *  trailing ellipsis dropped, then the list's `control_name_of` and the issue's key ("Saving
 *  labels of CORE-1"), or those words alone with no key. The words are the page's list's, passed
 *  in rather than imported: the page names the control outside React from the list it imports,
 *  and this file imports nothing of that list (`components/tooltips.tsx`). */
export function labelsControlName(
  issueKey: string | undefined,
  inside: {
    readonly labels: readonly string[];
    readonly saving: boolean;
    readonly words: Pick<PageWords, "ANSWER_LINES" | "CONTROL_LABELS">;
  } | null
): string {
  if (inside === null) return editLabelsName;
  const { words } = inside;
  if (inside.saving) {
    const said = withoutEllipsis(words.ANSWER_LINES.saving_labels);
    return issueKey === undefined
      ? said
      : `${said} ${words.CONTROL_LABELS.control_name_of} ${issueKey}`;
  }
  if (inside.labels.length > 0) return editLabelsName;
  return issueKey === undefined
    ? words.CONTROL_LABELS.add_label
    : `${words.CONTROL_LABELS.add_label} ${words.CONTROL_LABELS.control_name_to} ${issueKey}`;
}

export interface IssueLabelsProps {
  /** The shape of each label's chip, for a host that draws labels in a shape of its own (the
   *  new issue page's Details draws the prototype's `.lchip`). Given, the labels wrap. */
  readonly chipClassName?: string;
  readonly disabled: boolean;
  /** The issue's key, `labelsControlName`'s first argument, which ends the control's name inside
   *  `NoTooltips` while the control draws no label (`displayLabels`: the open popover's draft,
   *  else `labels`) or a save is in flight. Outside `NoTooltips` it changes nothing. */
  readonly issueKey?: string;
  readonly labels: readonly string[];
  readonly onSave: (labels: string[]) => Promise<unknown>;
  readonly project: string;
  readonly saving: boolean;
  readonly variant?: "default" | "rail";
}

/** The issue header's labels: the current chips and the shared `MultiSelect`, which stages a
 *  draft while open and saves the final draft once, when it closes. Its groups, in this order:
 *  the chips with their "+N" (outside `NoTooltips`, at most 10 before it), then the control that
 *  edits them. Inside `NoTooltips` (`components/tooltips.tsx`) it says the new issue page's words
 *  with their marks. */
export function IssueLabels({
  chipClassName,
  disabled,
  issueKey,
  labels,
  onSave,
  project,
  saving,
  variant = "default",
}: IssueLabelsProps): ReactNode {
  const page = usePageWords();
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState<string[]>([]);
  const [inputError, setInputError] = useState<string | null>(null);
  // The same list the project's issues table and IssueBoard load for this project, so opening
  // the popover on a page that already has it fetches nothing.
  const issues = useQuery({ ...projectIssuesQuery(project, []), enabled: open });
  const options = useMemo(
    () =>
      uniqueLabels([
        ...draft,
        ...labels,
        ...(issues.data ?? []).flatMap((issue) => issue.labels ?? []),
      ]).sort((left, right) => {
        const selectedOrder =
          Number(includesLabel(draft, right)) - Number(includesLabel(draft, left));
        return selectedOrder === 0 ? left.localeCompare(right) : selectedOrder;
      }),
    [draft, issues.data, labels]
  );
  const displayLabels = open ? draft : labels;
  // Inside `NoTooltips` the control's name begins with the words it draws: "Add a label" with no
  // label (below), as the priority's "Add a priority to CORE-1" does, and "Saving labels…" while it
  // saves, so a voice user says what they read (WCAG 2.5.3 Label in Name).
  // `labelsControlName` builds that name, and the page finds the control by it. Given `null`
  // everywhere outside, it is `main`'s name.
  const triggerName = labelsControlName(
    issueKey,
    page === null ? null : { labels: displayLabels, saving, words: page }
  );

  const setOpenState = useCallback(
    (next: boolean) => {
      if (next) {
        if (disabled || saving) return;
        setDraft([...labels]);
        setInputError(null);
        setOpen(true);
        return;
      }
      if (!open) return;
      const nextLabels = saveLabels(draft, labels);
      setOpen(false);
      setInputError(null);
      if (!labelsMatch(nextLabels, labels)) {
        void onSave(nextLabels).catch(() => undefined);
      }
    },
    [disabled, draft, labels, onSave, open, saving]
  );
  const changeDraft = (next: string[]) => {
    if (next.length > maxLabels) {
      setInputError(`An issue can have at most ${maxLabels} labels.`);
      return;
    }
    setDraft(next);
    setInputError(null);
  };
  const createLabel = (value: string) => {
    const label = value.trim();
    if (label.length === 0 || label.length > maxLabelLength) {
      setInputError(`Labels must be 1–${maxLabelLength} characters.`);
      return;
    }
    if (includesLabel(draft, label)) {
      setInputError(null);
      return;
    }
    changeDraft([...draft, label]);
  };
  const chips = displayLabels.map((label) => (
    <span
      className={
        page === null
          ? chipClassName === undefined
            ? `inline-flex min-h-11 max-w-full items-center rounded-full px-3 py-1 text-xs font-medium [overflow-wrap:anywhere] sm:min-h-7 sm:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface}`
            : `inline-flex shrink-0 items-center whitespace-nowrap ${chipClassName}`
          : chipClassName === undefined
            ? `inline-flex min-h-11 max-w-full items-center rounded-full px-3 py-1 text-xs font-medium [overflow-wrap:anywhere] sm:min-h-7 sm:px-2 ${pageLabelChip}`
            : `inline-flex max-w-full items-center [overflow-wrap:anywhere] ${chipClassName}`
      }
      key={label}
      {...page?.authorText}
    >
      {label}
    </span>
  ));

  return (
    <>
      <div
        className={
          variant === "rail"
            ? "relative flex min-w-0 flex-wrap items-center gap-1"
            : "relative mt-2 flex flex-wrap items-center gap-2"
        }
        data-testid="issue-labels"
      >
        {page === null ? (
          <CappedGroup className="contents" label="labels" name="labels">
            {chips}
          </CappedGroup>
        ) : (
          chips
        )}
        <MultiSelect
          // Not disabled while it saves what the close committed: a disabled button cannot hold
          // focus, so Escape's return to the trigger would be undone by the save's own render.
          // `setOpenState` is what refuses to reopen until the save has landed, and `aria-disabled`
          // is how the reader is told it is busy without focus being taken away.
          ariaDisabled={saving}
          disabled={disabled}
          emptyMessage="No labels yet."
          error={inputError}
          label="Labels"
          onChange={changeDraft}
          onCreate={createLabel}
          onOpenChange={setOpenState}
          onQueryChange={() => setInputError(null)}
          open={open}
          options={options}
          searchLabel="Search or create label"
          selected={draft}
          triggerAriaLabel={triggerName}
          // Inside `NoTooltips` each colour is one of the new issue page's: a label above is
          // its label chip (the prototype's `.lchip`), and this trigger its secondary control, the
          // header control's rule and muted words on the paper, with quiet words while it is busy.
          triggerClassName={`min-h-11 shrink-0 rounded-lg border px-3 py-1 text-xs font-medium whitespace-nowrap sm:min-h-7 sm:px-2 ${
            page === null
              ? `${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`
              : `${pageLineBorder} ${pagePaperBg} ${pageMutedText} refused:text-page-quiet`
          }`}
        >
          {page === null ? (
            saving ? (
              "Saving labels…"
            ) : displayLabels.length === 0 ? (
              "Labels"
            ) : (
              "+"
            )
          ) : (
            <span {...page.pageChrome}>
              {/* With no label the control says what it does, as the priority's says "Add a
                  priority": its row of Details already names the property, so the property's
                  name here drew "Labels" twice. */}
              {saving
                ? page.ANSWER_LINES.saving_labels
                : displayLabels.length === 0
                  ? page.CONTROL_LABELS.add_label
                  : "+"}
            </span>
          )}
        </MultiSelect>
      </div>
      {/* A refused save shows in the shell's pending-writes indicator, with every other write's
          (`PendingWritesIndicator`), and the popover's draft is gone with its close. */}
    </>
  );
}
