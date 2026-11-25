import { useQuery } from "@tanstack/react-query";
import {
  Fragment,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, useInRouterContext } from "react-router-dom";

import { api } from "../../api/client";
import { issueQuery, whoAmIQuery } from "../../api/queries";
import type { GoingPlanAnswer, GoingPlanRow, GoingUndoLine } from "../../api/types";
import { Pill, pillShapeClassName } from "../../components/Pill";
import { QueryError } from "../../components/QueryError";
import { issueProject } from "../../lib/issue-key";
import {
  borderDefault,
  borderStrong,
  borderTransparent,
  calloutDangerBorder,
  calloutWarningBorder,
  dangerText,
  diffAddedBg,
  diffAddedText,
  diffRemovedBg,
  diffRemovedText,
  focusVisibleRing,
  inlineWarningText,
  inputClasses,
  linkHoverText,
  linkText,
  primaryButton,
  quoteAccentBorder,
  quoteBodyText,
  secondaryButtonCompact,
  secondaryButtonPaired,
  successText,
  surfaceBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { changedWords, type WordPart } from "../doc/change-set";
import { AssigneeEditor } from "../issue/AssigneeControl";
import { IssueLabels } from "../issue/IssueLabels";
import { issuePriority } from "../issue/issue-priority";
import { PriorityEditor } from "../issue/PriorityControl";
import { StatusMenu } from "../issue-page/StatusMenu";
import { issueStatuses } from "../project/board-model";
import { AgentName, useAgentText } from "../refs/AgentName";
import { TimedWords } from "../refs/dispatch-time";
import { ItemChip } from "../refs/ItemChip";
import { objectOf, textOf } from "./fields";
import { draftOf, type PlanDraft, setDraft, useGoing, useGoingValue } from "./going-store";
import { PriorityValue } from "./PriorityValue";
import {
  approvable,
  argumentsOf,
  broadcastSessions,
  type ChangePiece,
  cardTitle,
  type DocumentChange,
  type DocumentNaming,
  deletedSince,
  deliveryWords,
  documentChange,
  documentNaming,
  editedDraft,
  editPreview,
  fieldWords,
  irreversibleWords,
  type LabelMark,
  labelMarks,
  MARK_WORDS,
  type MessageReach,
  MODE_WORDS,
  messageReach,
  needsUnticked,
  newIssueTitles,
  planWaits,
  postAction,
  postGone,
  postRead,
  ROW_MARKS,
  type RowChange,
  type RowMark,
  reachesNoSession,
  removalWords,
  reviewWords,
  rowChanges,
  rowKind,
  rowLabel,
  rowOutcome,
  rowRemoves,
  rowResultMessage,
  rowText,
  rowWaits,
  specSentences,
  tickedCount,
  toggledDraft,
  undoButtonWords,
  undoRecord,
  untickedRows,
  valuesNowWords,
  valueWords,
  wasReverted,
  withChangesUnticked,
} from "./plan-rows";
import { postToolOf } from "./post-tools";
import { focusConversationBox } from "./RunControls";
import { UndoResult, useRunUndo } from "./RunUndo";
import type { PlanAnswering } from "./usePlan";

/** Each mark of a row as it draws (design v2, state 7): a ring while it waits its turn, a
 *  spinner while it runs, a check once it ran, a cross where it failed, a dash where it did not
 *  run, and a grey revert arrow - a hooked arrow back, which no spinner looks like - never a
 *  check, once Undo took its change back. Motion stops where the person asked for less. */
const MARK_GLYPHS: Readonly<Record<Exclude<RowMark, "waits">, ReactNode>> = {
  done: <span className={successText}>✓</span>,
  failed: <span className={dangerText}>✕</span>,
  not_run: <span className={textMutedOnSurface}>–</span>,
  reverted: (
    <svg
      aria-hidden="true"
      className={`inline-block size-3.5 ${textMutedOnSurface}`}
      data-testid="going-plan-reverted-mark"
      fill="none"
      stroke="currentColor"
      strokeLinecap="round"
      strokeLinejoin="round"
      strokeWidth="1.75"
      viewBox="0 0 16 16"
    >
      <path d="M6 3.5 3 6.5l3 3" />
      <path d="M3 6.5h6.5a3.5 3.5 0 0 1 0 7H7" />
    </svg>
  ),
  running: (
    <span className="inline-block size-3 rounded-full border-2 border-current border-t-transparent motion-safe:animate-spin" />
  ),
  turn: <span className={textMutedOnSurface}>○</span>,
};

/** Where the person sets which changes of a plan wait for them: the Command bar section of
 *  Settings (`CommandBarSection.tsx`). */
const MODE_SETTING_PATH = "/settings#command-bar-heading";

/** A row of a plan card or of the page's planned changes. */
const ROW = '[data-testid="going-plan-row"]';

/** How the card takes the focus from the box while it shows a plan that waits (`holdCardFocus`),
 *  and the box it hands the focus back to (`usePlanCardKeys`). */
let cardFocus: (() => boolean) | undefined;
let cardBox: HTMLElement | undefined;

/** Holds `take` as the way the box's ↑ moves the focus into the card, until the release it
 *  answers. */
function holdCardFocus(take: () => boolean): () => void {
  cardFocus = take;
  return () => {
    if (cardFocus === take) cardFocus = undefined;
  };
}

/**
 * What the box does with a key while a plan waits (design v3, Keys, "Bar, a plan waits"): ↑ in
 * the empty box moves the focus from the box into the plan card, onto its row nearest the box.
 * There ↑ ↓ move between rows, Space ticks or unticks a row, and E opens the editor of its new
 * value; Esc, or any letter, puts the focus back in this box, the letter typed into it. The
 * handler answers whether the key was the card's, which the box then does nothing more with.
 */
export function usePlanCardKeys(): (
  event: KeyboardEvent<HTMLInputElement | HTMLTextAreaElement>
) => boolean {
  return useCallback((event) => {
    const box = event.currentTarget;
    if (
      event.key !== "ArrowUp" ||
      event.altKey ||
      event.ctrlKey ||
      event.metaKey ||
      event.shiftKey ||
      box.value !== ""
    ) {
      return false;
    }
    cardBox = box;
    if (cardFocus === undefined || !cardFocus()) return false;
    event.preventDefault();
    return true;
  }, []);
}

/** Opens the editor of a row's first new value the person can edit: the issue page's priority
 *  menu, its label picker, or a box of text. */
function openEditor(row: HTMLElement): void {
  const editor = row.querySelector<HTMLElement>("[data-plan-edit]");
  const select = editor?.querySelector("select");
  if (select !== null && select !== undefined) {
    select.focus();
    // A browser that opens no menu from a key leaves the menu focused, where Space opens it.
    if ("showPicker" in select && typeof select.showPicker === "function") {
      try {
        select.showPicker();
      } catch {
        // The menu stays focused.
      }
    }
    return;
  }
  editor?.click();
}

/** The card's keys on its rows (`usePlanCardKeys`). A key pressed in a control of a row - an edit
 *  box, the priority menu, the label picker - is that control's. */
function onRowKey(event: KeyboardEvent<HTMLUListElement>): void {
  const row = event.target;
  if (
    !(row instanceof HTMLElement) ||
    !row.matches(ROW) ||
    event.altKey ||
    event.ctrlKey ||
    event.metaKey
  ) {
    return;
  }
  const box = cardBox?.isConnected === true ? cardBox : undefined;
  switch (event.key) {
    case "ArrowDown":
    case "ArrowUp": {
      event.preventDefault();
      const rows = [...event.currentTarget.querySelectorAll<HTMLElement>(ROW)];
      const at = rows.indexOf(row);
      rows[
        event.key === "ArrowDown" ? Math.min(at + 1, rows.length - 1) : Math.max(at - 1, 0)
      ]?.focus();
      return;
    }
    case " ":
      event.preventDefault();
      row.querySelector<HTMLInputElement>('input[type="checkbox"]')?.click();
      return;
    case "e":
    case "E":
      event.preventDefault();
      openEditor(row);
      return;
    case "Escape":
      if (box !== undefined) {
        event.preventDefault();
        event.stopPropagation();
        box.focus();
      }
      return;
    default:
      // The letter is not taken: it goes on to the box the focus is now in.
      if (box !== undefined && /^\p{L}$/u.test(event.key)) box.focus();
  }
}

/**
 * The person's answer to `plan` as the store holds it, with each row whose item changed after the
 * plan showed unticked once for that change (`withChangesUnticked`), so it waits to be ticked
 * again (R4). Undefined while no plan is read.
 */
export function usePlanDraft(plan: GoingPlanAnswer | undefined): PlanDraft | undefined {
  const held = useGoingValue((of) => of.draft);
  const draft = plan === undefined ? undefined : draftOf(plan.run, plan.plan, held);
  useEffect(() => {
    if (plan === undefined || draft === undefined) return;
    const unticked = withChangesUnticked(draft, plan);
    if (unticked !== draft) setDraft(unticked);
  }, [plan, draft]);
  return draft;
}

/** Word runs marked as a diff marks them: those taken out struck through in red, those put in in
 *  green, the rest as they are. */
function MarkedParts({ parts }: { readonly parts: readonly WordPart[] }): ReactNode {
  return parts.map((part, index) => {
    const Mark = part.kind === "added" ? "ins" : part.kind === "removed" ? "del" : "span";
    const marked =
      part.kind === "added"
        ? `${diffAddedBg} ${diffAddedText} no-underline`
        : part.kind === "removed"
          ? `${diffRemovedBg} ${diffRemovedText}`
          : undefined;
    return (
      // biome-ignore lint/suspicious/noArrayIndexKey: a part is its place in the diff, which never changes for one piece
      <Mark className={marked} key={index}>
        {part.value}
      </Mark>
    );
  });
}

/** The words of one change marked as a diff marks them (`changedWords`, `MarkedParts`). Each
 *  pair is diffed once, within the bound `changedWords` keeps. */
function MarkedWords({ added, removed }: ChangePiece): ReactNode {
  const parts = useMemo(() => changedWords(removed, added), [removed, added]);
  return <MarkedParts parts={parts} />;
}

/** One piece of a document change, its words out and in marked as a diff marks them. */
function PieceDiff({ piece }: { readonly piece: ChangePiece }): ReactNode {
  return (
    <p
      className="whitespace-pre-wrap break-words font-mono text-xs"
      data-testid="going-plan-diff-piece"
    >
      <MarkedWords added={piece.added} removed={piece.removed} />
    </p>
  );
}

/** The longest a post's body reads whole in its row's inline diff, in characters: two lines of
 *  the bar's row. */
const INLINE_BODY_MAX = 160;

/**
 * An edit of a post's body (review a3), drawn as VS Code's inline diff draws a change: the body
 * once, the words the edit takes out struck through in red and the words it puts in in green,
 * the rest as they are, so the change is read and not hunted for between two copies; then the
 * line's Edit and "Edited" (`trailing`). A long body - more than one line, or past
 * `INLINE_BODY_MAX` - reads as two or three of its sentences with the words the edit changes
 * marked the same way (`editPreview`, review m1), and its whole diff under them, closed, as a
 * document's text change has (R4); its Edit and "Edited" stand on the diff's line, as an Edit
 * stands on the line of what it edits in every other row (review m2).
 */
function BodyDiff({
  next,
  now,
  trailing,
}: {
  readonly next: string;
  readonly now: string;
  readonly trailing: ReactNode;
}): ReactNode {
  const long = [now, next].some((body) => body.includes("\n") || body.length > INLINE_BODY_MAX);
  const preview = useMemo(() => (long ? editPreview(now, next) : []), [long, now, next]);
  if (!long) {
    return (
      <>
        <span className={textSecondaryOnSurface} data-testid="going-plan-next">
          <span className="whitespace-pre-wrap break-words" data-testid="going-plan-body-diff">
            <MarkedWords added={next} removed={now} />
          </span>
        </span>
        {trailing}
      </>
    );
  }
  return (
    <span className={`block w-full ${textSecondaryOnSurface}`} data-testid="going-plan-next">
      <span className="block space-y-1" data-testid="going-plan-body-diff">
        <span className="block break-words" data-testid="going-plan-sentences">
          <MarkedParts parts={preview} />
        </span>
        {/* The diff gives way to Edit, so an open diff keeps Edit on its line too. */}
        <span className="flex items-baseline gap-x-2">
          <details className="min-w-0" data-testid="going-plan-diff">
            <summary className={`cursor-pointer text-xs ${textMutedOnSurface}`}>The diff</summary>
            <PieceDiff piece={{ added: next, removed: now }} />
          </details>
          {trailing}
        </span>
      </span>
    </span>
  );
}

/** The issue a row changes, as an answer names an item (design v3, state 6): its chip, with its
 *  key, a dot in its status's colour and its title (`ItemChip`), on one line, its title cut with
 *  "…" where the row is narrower than it. */
function ItemSlot({ item }: { readonly item: string }): ReactNode {
  return (
    <span className="min-w-0" data-item={item} data-testid="going-plan-item">
      {item === "" ? null : <ItemChip issue={item} />}
    </span>
  );
}

/** Labels as the issue page's chips: a label the change keeps, one it puts in, in green, and one
 *  it takes out, struck through; "None" for no label. */
function LabelChips({ marks }: { readonly marks: readonly LabelMark[] }): ReactNode {
  if (marks.length === 0) return <>{valueWords("labels", [])}</>;
  return (
    <span className="inline-flex flex-wrap items-center gap-1">
      {marks.map(({ label, mark }) =>
        mark === "added" ? (
          <ins
            className={`${pillShapeClassName} ${diffAddedBg} ${diffAddedText} no-underline`}
            data-mark="added"
            key={label}
          >
            <span className="sr-only">added </span>
            {label}
          </ins>
        ) : mark === "removed" ? (
          <del
            className={`${pillShapeClassName} ${diffRemovedBg} ${diffRemovedText}`}
            data-mark="removed"
            key={label}
          >
            <span className="sr-only">removed </span>
            {label}
          </del>
        ) : (
          <Pill key={label}>{label}</Pill>
        )
      )}
    </span>
  );
}

/** A value as the issue page shows it: labels as its chips, marked against the labels `against`
 *  where given; a priority as its badge (`PriorityValue`); a session holding a claim by its name;
 *  anything else in words (`valueWords`). */
function FieldValue({
  against,
  field,
  newIssues,
  value,
}: {
  readonly against?: unknown;
  readonly field: string;
  readonly newIssues: ReadonlyMap<number, string> | undefined;
  readonly value: unknown;
}): ReactNode {
  if (field === "labels" && Array.isArray(value)) {
    return <LabelChips marks={labelMarks(against ?? value, value)} />;
  }
  if (field === "priority") {
    return <PriorityValue priority={issuePriority(typeof value === "number" ? value : null)} />;
  }
  const holder = objectOf(value);
  if (field === "claim" && holder.kind === "session") {
    return <AgentName kind="static" sessionId={textOf(holder.id)} storedTitle={undefined} />;
  }
  return <>{valueWords(field, value, newIssues)}</>;
}

/** The issue page's label picker over the labels a row writes, opened at once; the labels it
 *  closes on are the person's edit. */
function LabelsEditor({
  labels,
  onEdit,
  project,
}: {
  readonly labels: readonly string[];
  readonly onEdit: (labels: string[]) => void;
  readonly project: string;
}): ReactNode {
  const holder = useRef<HTMLSpanElement>(null);
  useEffect(() => {
    holder.current?.querySelector<HTMLButtonElement>('button[aria-haspopup="listbox"]')?.click();
  }, []);
  return (
    <span data-testid="going-plan-labels-editor" ref={holder}>
      <IssueLabels
        disabled={false}
        labels={labels}
        onSave={async (next) => onEdit(next)}
        project={project}
        saving={false}
        variant="rail"
      />
    </span>
  );
}

/** A new value the person can press to edit, as the issue page lets them press a value. */
const VALUE_BUTTON = `rounded text-left underline decoration-dotted underline-offset-2 outline-none focus-visible:ring-2 ${focusVisibleRing} ${textPrimaryOnSurface}`;

/**
 * One field a row writes, in the issue page's words (design v3, rule 6): as a line, its name, the
 * value now in the bar's grey, which no reader takes for a link, and the new value ("Priority: No
 * priority → P2"), each value in one look - a priority as its badge, a status by its word, labels
 * as chips - whatever the field; as a new issue's title, the value alone; as a part of a new
 * issue's line, the value with the word that says what it is ("under CORE-1"); as a post's body
 * (`diff`), the body once with the words the edit changes marked (`BodyDiff`) and no field name,
 * since the row already says it is a comment (review a3). While the row waits, each line the
 * person may edit has one Edit, whatever its field, which swaps the new value for the field's own
 * control from the issue page and opens it: the priority menu, the label picker, the status menu,
 * the assignee picker, or a box of text; a pick puts the value back in its words. A title or a
 * part of a new issue's line is pressed to edit it instead, and a title also says Edit. A value of
 * a field that may name a new issue of the plan (`RowChange.namesNewIssue`) naming one shows that
 * issue's title and "(new)" (`newIssues`), and its edit box holds the value itself, which the
 * person may replace with an issue's key. The edit box starts from the new value of the plan it
 * is drawn for, and its caller keys it to that plan, so a revised plan or a new run's never opens
 * with an earlier plan's value or the person's unsaved typing. An edited value says "Edited".
 * Once Undo took the change back, a line reads as the plan card drew it, "Priority: No priority →
 * P2", with no strike: the row's own status says it was reverted (review N3).
 */
function FieldChange({
  change,
  editable,
  issueKey,
  newIssues,
  onEdit,
  project,
  shape,
}: {
  readonly change: RowChange;
  readonly editable: boolean;
  readonly issueKey: string;
  readonly newIssues: ReadonlyMap<number, string>;
  readonly onEdit: (value: unknown) => void;
  readonly project: string;
  readonly shape: "line" | "title" | "part" | "diff";
}): ReactNode {
  const { field, next, now } = change;
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState(typeof next === "string" ? next : "");
  const box = useRef<HTMLInputElement>(null);
  const line = useRef<HTMLDivElement>(null);
  const named = fieldWords(field).toLowerCase();
  const shown = change.namesNewIssue ? newIssues : undefined;
  const edit = editable ? change.edit : undefined;
  // A line's Edit swaps its value for the field's control, which opens at once: a menu's select
  // shows its options, a picker its list, and a box of text takes the focus.
  const lineEdit = (shape === "line" || shape === "diff") && edit !== undefined;
  useEffect(() => {
    if (!editing) return;
    if (box.current !== null) {
      box.current.focus();
      return;
    }
    if (!lineEdit) return;
    const select = line.current?.querySelector("select");
    if (select === null || select === undefined) {
      line.current?.querySelector<HTMLButtonElement>("[data-plan-control] button")?.click();
      return;
    }
    select.focus();
    if ("showPicker" in select && typeof select.showPicker === "function") {
      try {
        select.showPicker();
      } catch {
        // A browser that opens no menu from a script leaves the menu focused, where Space opens it.
      }
    }
  }, [editing, lineEdit]);
  const submit = (value: unknown) => {
    onEdit(value);
    setEditing(false);
  };
  const nextValue = <FieldValue against={now} field={field} newIssues={shown} value={next} />;
  // A post's body that is not being edited is drawn with its line's Edit and "Edited" in it, where
  // its length puts them (`BodyDiff`); every other value has them after it.
  const inBody = shape === "diff" && !editing;
  const trailing = (
    <>
      {lineEdit && !editing ? (
        <>
          {" "}
          <button
            aria-label={`Edit ${named}`}
            className={secondaryButtonCompact}
            data-plan-edit=""
            onClick={() => setEditing(true)}
            type="button"
          >
            Edit
          </button>
        </>
      ) : null}
      {change.edited ? (
        <>
          {" "}
          <span className={`font-medium ${textMutedOnSurface}`} data-testid="going-plan-edited">
            Edited
          </span>
        </>
      ) : null}
    </>
  );
  let value: ReactNode;
  if (edit === "priority" && (editing || (!lineEdit && next !== null))) {
    value = (
      <span data-plan-control="" data-plan-edit="" data-testid="going-plan-next">
        <PriorityEditor
          issueKey={issueKey}
          write={{
            failed: false,
            pending: false,
            retry: undefined,
            shown: issuePriority(typeof next === "number" ? next : null),
            stopped: undefined,
            submit,
          }}
        />
      </span>
    );
  } else if (edit === "status" && (editing || !lineEdit)) {
    value = (
      <span data-plan-control="" data-plan-edit="" data-testid="going-plan-next">
        <StatusMenu
          label={`Status of ${issueKey}`}
          onPick={submit}
          status={typeof next === "string" ? next : ""}
          statuses={issueStatuses}
        />
      </span>
    );
  } else if (edit === "assignee" && (editing || !lineEdit)) {
    value = (
      <span data-plan-control="" data-plan-edit="" data-testid="going-plan-next">
        <AssigneeEditor
          issueKey={issueKey}
          write={{
            error: undefined,
            failed: false,
            pending: false,
            retry: undefined,
            shown: typeof next === "string" && next !== "" ? next : null,
            stopped: undefined,
            submit,
          }}
        />
      </span>
    );
  } else if (edit === "labels" && editing) {
    value = (
      <LabelsEditor
        labels={Array.isArray(next) ? next.filter((label) => typeof label === "string") : []}
        onEdit={submit}
        project={project}
      />
    );
  } else if (edit === "text" && editing) {
    value = (
      <form
        className="flex items-center gap-1"
        onSubmit={(event) => {
          event.preventDefault();
          submit(text);
        }}
      >
        <input
          aria-label={`New ${named}`}
          className={`rounded border px-1 py-0.5 text-xs ${inputClasses(true)}`}
          onChange={(event) => setText(event.target.value)}
          onKeyDown={(event) => {
            // Enter keeps the edit, and the plan is approved from the box alone.
            event.stopPropagation();
            if (event.key === "Escape") setEditing(false);
          }}
          ref={box}
          value={text}
        />
        <button className={secondaryButtonCompact} type="submit">
          Keep
        </button>
      </form>
    );
  } else if (edit !== undefined && !lineEdit) {
    // A part of a new issue's line is pressed to edit it; a title also says Edit.
    value = (
      <>
        <button
          aria-label={
            shape === "part" ? `Edit ${named}: ${valueWords(field, next, shown)}` : undefined
          }
          className={VALUE_BUTTON}
          data-testid="going-plan-next"
          onClick={() => setEditing(true)}
          type="button"
        >
          {nextValue}
        </button>
        {shape === "part" ? null : (
          <>
            {" "}
            <button
              aria-label={`Edit ${named}`}
              className={secondaryButtonCompact}
              data-plan-edit=""
              onClick={() => setEditing(true)}
              type="button"
            >
              Edit
            </button>
          </>
        )}
      </>
    );
  } else if (inBody) {
    value = <BodyDiff next={textOf(next)} now={textOf(now)} trailing={trailing} />;
  } else {
    value = (
      <span className={textPrimaryOnSurface} data-testid="going-plan-next">
        {nextValue}
      </span>
    );
  }
  return (
    <div
      className={`flex flex-wrap items-baseline gap-x-2 gap-y-1 ${shape === "title" ? "text-sm font-medium" : shape === "diff" ? "text-sm" : "text-xs"}`}
      data-field={field}
      data-testid="going-plan-field"
      ref={line}
    >
      {shape === "line" ? (
        <>
          <span className={`font-medium ${textSecondaryOnSurface}`}>{fieldWords(field)}:</span>{" "}
          <span className={textMutedOnSurface} data-testid="going-plan-now">
            <FieldValue field={field} newIssues={undefined} value={now} />
          </span>{" "}
          <span aria-hidden="true">→</span>{" "}
        </>
      ) : null}
      {shape === "part" && field === "parent" ? (
        <>
          <span className={textMutedOnSurface}>under</span>{" "}
        </>
      ) : null}
      {value}
      {inBody ? null : trailing}
    </div>
  );
}

/** The words a row posts or sends, whole, as a quote; struck through where the row deletes them,
 *  under a grey bar rather than a message's blue one, which reads as words that go out (review
 *  a2). */
function Quote({
  struck = false,
  text,
}: {
  readonly struck?: boolean;
  readonly text: string;
}): ReactNode {
  return (
    <blockquote
      className={`border-l-2 pl-2 text-sm whitespace-pre-wrap break-words ${struck ? borderStrong : quoteAccentBorder} ${quoteBodyText}`}
      data-testid="going-plan-quote"
    >
      {struck ? <del>{text}</del> : text}
    </blockquote>
  );
}

/** The parts of a sentence joined as a list is: "a", "a and b", "a, b and c". */
function Listed({ parts }: { readonly parts: readonly ReactNode[] }): ReactNode {
  return parts.map((part, index) => (
    // biome-ignore lint/suspicious/noArrayIndexKey: a part is its place in the one sentence, which never changes for one row
    <Fragment key={index}>
      {index === 0 ? null : index === parts.length - 1 ? " and " : ", "}
      {part}
    </Fragment>
  ));
}

/** The first line of a message row, in the one form every message row has (design v3, state 6):
 *  whom it reaches (`to`, "Message to ‹session› on DEMO-15", "Message to 4 sessions"), then how
 *  where the row names a delivery ("· as an aside"); and under it the sessions (`sessions`) of a
 *  line that names them by their count. */
function MessageTo({
  delivery,
  sessions,
  to,
}: {
  readonly delivery: string;
  readonly sessions: readonly string[];
  readonly to: ReactNode;
}): ReactNode {
  return (
    <>
      <p className={textPrimaryOnSurface} data-testid="going-plan-message">
        {to}
        {delivery === "" ? null : ` · ${delivery}`}
      </p>
      {sessions.length === 0 ? null : (
        <ul className={`text-xs ${textSecondaryOnSurface}`} data-testid="going-plan-sessions">
          {sessions.map((session) => (
            <li key={session}>
              <AgentName kind="static" sessionId={session} storedTitle={undefined} />
            </li>
          ))}
        </ul>
      )}
    </>
  );
}

/** Whom a message on `issue`'s conversation reaches (`messageReach`), read from the issue as it
 *  stands and from the sessions that follow it, under the key the issue's header and an ask's
 *  card read them by, so a session that stops following it leaves all three. */
function useIssueReach(issue: string): MessageReach {
  const read = useQuery(issueQuery(issue)).data;
  const followers = useQuery({
    queryFn: () => api.getIssueSubscribers(issue),
    queryKey: ["subscribers", issue],
  }).data;
  return messageReach(
    read,
    followers?.map((follower) => follower.session_id)
  );
}

/**
 * Whom a message on an issue's conversation, `row`, reaches (`useIssueReach`), in the form a
 * message to a session and a broadcast have, while the plan waits and once it ran alike: one
 * session by its name, "Message to ‹session› on DEMO-15"; more by their count, "Message to 3
 * sessions on DEMO-15", with the sessions under it; a role no session is known to hold by its
 * name, "the planner"; and none, "Message to no session on DEMO-15", where the page and the server
 * both know it reaches none (`reachesNoSession`). Where the page cannot read who follows the
 * issue, or the server finds a session the page does not know, it says whom Dispatch tells of it
 * in words: "Message to the agents that follow DEMO-15", after whoever the issue's own read names.
 * A message on an issue names no delivery, so the line says none.
 */
function IssueMessageTo({
  delivery,
  issue,
  row,
}: {
  readonly delivery: string;
  readonly issue: string;
  readonly row: GoingPlanRow;
}): ReactNode {
  const reach = useIssueReach(issue);
  const chip = <ItemSlot item={issue} />;
  const [only] = reach.sessions;
  const counted = reach.sessions.length > 1;
  const named: ReactNode[] = reach.role === undefined ? [] : [`the ${reach.role}`];
  if (counted) {
    named.push(`${reach.sessions.length} sessions`);
  } else if (only !== undefined) {
    named.push(<AgentName kind="static" sessionId={only} storedTitle={undefined} />);
  } else if (reachesNoSession(reach, row)) {
    named.push("no session");
  }
  const followersInWords = !reach.followersKnown || named.length === 0;
  return (
    <MessageTo
      delivery={delivery}
      sessions={counted ? reach.sessions : []}
      to={
        followersInWords ? (
          <>
            Message to <Listed parts={[...named, <>the agents that follow {chip}</>]} />
          </>
        ) : (
          <>
            Message to <Listed parts={named} /> on {chip}
          </>
        )
      }
    />
  );
}

/**
 * A message row (R7): whom it reaches and how - "Message to ‹session› on DEMO-15 · as a BTW", a
 * broadcast's "Message to 4 sessions · as an aside" with the sessions under it, a message on an
 * issue's conversation by whom Dispatch delivers it to (`IssueMessageTo`) - and the whole text as
 * a quote. While it waits it says that a message cannot be undone once sent (R6).
 */
function MessageBody({
  row,
  waiting,
}: {
  readonly row: GoingPlanRow;
  readonly waiting: boolean;
}): ReactNode {
  const args = argumentsOf(row);
  const delivery = deliveryWords(row);
  const onIssue = row.item || row.issue || textOf(args.issue);
  let to: ReactNode;
  if (row.tool === "dispatch_broadcast") {
    const sessions = broadcastSessions(row);
    to = (
      <MessageTo
        delivery={delivery}
        sessions={sessions}
        to={`Message to ${sessions.length} ${sessions.length === 1 ? "session" : "sessions"}`}
      />
    );
  } else if (row.tool === "dispatch_send_to_agent") {
    to = (
      <MessageTo
        delivery={delivery}
        sessions={[]}
        to={
          <>
            Message to{" "}
            <AgentName kind="static" sessionId={textOf(args.session)} storedTitle={undefined} />
            {row.issue === "" ? null : (
              <>
                {" "}
                on <ItemSlot item={row.issue} />
              </>
            )}
          </>
        }
      />
    );
  } else if (onIssue === "") {
    to = <MessageTo delivery={delivery} sessions={[]} to="Message in its conversation" />;
  } else {
    to = <IssueMessageTo delivery={delivery} issue={onIssue} row={row} />;
  }
  return (
    <div className="space-y-1">
      {to}
      <Quote text={rowText(row)} />
      {waiting ? (
        <p className={`text-xs ${textMutedOnSurface}`} data-testid="going-plan-final">
          Cannot be undone once sent
        </p>
      ) : null}
    </div>
  );
}

/**
 * What a message on an issue's conversation, `row`, says once it was sent (R7): where it went,
 * "Posted in the issue's conversation", where it reaches no session (`reachesNoSession`), since
 * "Sent" under "Message to no session" leaves the person asking to whom (review a8), and the
 * issue's chip stands in the line above it already (review N1); "Sent" where a session takes it.
 */
function SentOnIssue({
  issue,
  row,
}: {
  readonly issue: string;
  readonly row: GoingPlanRow;
}): ReactNode {
  const reach = useIssueReach(issue);
  return reachesNoSession(reach, row) ? "Posted in the issue's conversation" : "Sent";
}

/**
 * A row that edits or deletes a comment or an issue message: what it does to whose post on which
 * issue, "Edit the comment by alice on DEMO-15", or "Edit your comment on DEMO-15" where the post
 * is the signed-in person's own (review a3), the issue by its chip; then an edit's body once,
 * with the words it changes marked (`BodyDiff`) and Edit while the row waits, as every change of
 * a field has; or a delete's body, whole and struck through, and while it waits, that it cannot
 * be undone once deleted (R6), as a message cannot once sent. A post deleted after the plan read
 * it (`deletedSince`) shows no word of it, nor the words an edit would put in its place, which can
 * repeat it: "This comment was deleted." A delete that ran says so in its outcome.
 */
function PostBody({
  changes,
  field,
  row,
  waiting,
}: {
  readonly changes: readonly RowChange[];
  readonly field: (change: RowChange) => ReactNode;
  readonly row: GoingPlanRow;
  readonly waiting: boolean;
}): ReactNode {
  const read = postRead(row);
  const post = postToolOf(row.tool);
  const login = useQuery(whoAmIQuery()).data?.login;
  const yours = read.author?.kind === "user" && read.author.id === login;
  let below: ReactNode;
  if (deletedSince(row)) {
    below = (
      <p className={`text-xs ${textSecondaryOnSurface}`} data-testid="going-plan-post-deleted">
        This {post?.kind ?? "post"} was deleted.
      </p>
    );
  } else if (post?.deletes === true) {
    below = (
      <>
        {read.body === undefined ? null : <Quote struck text={read.body} />}
        {waiting ? (
          <p className={`text-xs ${textMutedOnSurface}`} data-testid="going-plan-final">
            Cannot be undone once deleted
          </p>
        ) : null}
      </>
    );
  } else {
    below = changes.map(field);
  }
  return (
    <div className="space-y-1">
      <p className={textPrimaryOnSurface} data-testid="going-plan-post">
        {postAction(row, yours)}
        {read.author === undefined || yours ? null : (
          <>
            {" by "}
            <AgentName actor={read.author} kind="static" />
          </>
        )}
        {row.issue === "" ? null : (
          <>
            {" on "}
            <ItemSlot item={row.issue} />
          </>
        )}
      </p>
      {below}
    </div>
  );
}

/** A change of a document's text (R4): what the row does, in the page's words drawn from the
 *  row's data (`documentNaming`), the issue whose document it changes by its chip after them -
 *  "Suggest a change to the spec of DEMO-15", "Upload notes.md to DEMO-15" - then two or three
 *  sentences on what it changes, and its diff under them, closed. */
function DocumentBody({
  document,
  naming,
}: {
  readonly document: DocumentChange;
  readonly naming: DocumentNaming;
}): ReactNode {
  return (
    <div className="space-y-1">
      <p className={textPrimaryOnSurface} data-testid="going-plan-document">
        {naming.words}
        {naming.issue === "" ? null : (
          <>
            {" "}
            <ItemSlot item={naming.issue} />
          </>
        )}
      </p>
      <p className={textSecondaryOnSurface} data-testid="going-plan-sentences">
        <TimedWords text={document.sentences} />
      </p>
      <details data-testid="going-plan-diff">
        <summary className={`cursor-pointer text-xs ${textMutedOnSurface}`}>The diff</summary>
        {document.pieces.map((piece, index) => (
          // biome-ignore lint/suspicious/noArrayIndexKey: a piece is its place in the change, which never changes for one row
          <PieceDiff key={index} piece={piece} />
        ))}
      </details>
    </div>
  );
}

/**
 * A new issue's row: a "New" chip and its title, then its fields on one line, "DEMO · under
 * DEMO-12 · P2", with its labels as chips; its short spec as two or three sentences, with the
 * whole text under them, closed. The title, the project and the parent are the person's to edit.
 */
function NewIssueBody({
  changes,
  field,
  spec,
}: {
  readonly changes: readonly RowChange[];
  readonly field: (change: RowChange, shape: "title" | "part") => ReactNode;
  readonly spec: string;
}): ReactNode {
  const title = changes.find((change) => change.field === "title");
  const parts = changes.filter((change) => change.field !== "title");
  return (
    <div className="space-y-1">
      <div className="flex flex-wrap items-baseline gap-2">
        <Pill>New</Pill>
        {title === undefined ? null : field(title, "title")}
      </div>
      {parts.length === 0 ? null : (
        <div
          className={`flex flex-wrap items-baseline gap-x-1 gap-y-1 text-xs ${textSecondaryOnSurface}`}
          data-testid="going-plan-new-issue-fields"
        >
          {parts.map((change, index) => (
            <Fragment key={change.field}>
              {index === 0 ? null : <span aria-hidden="true"> · </span>}
              {field(change, "part")}
            </Fragment>
          ))}
        </div>
      )}
      {spec.trim() === "" ? null : (
        <div className="space-y-1">
          <p className={textSecondaryOnSurface} data-testid="going-plan-spec-sentences">
            {specSentences(spec)}
          </p>
          <details data-testid="going-plan-spec">
            <summary className={`cursor-pointer text-xs ${textMutedOnSurface}`}>
              The whole spec
            </summary>
            <p className={`text-xs whitespace-pre-wrap break-words ${textSecondaryOnSurface}`}>
              {spec}
            </p>
          </details>
        </div>
      )}
    </div>
  );
}

/**
 * One row of a plan, wherever it is drawn (design v3, state 6): its tick while it waits, then the
 * change as its kind shows it - a change of an issue's fields as the item and each field in the
 * issue page's words with the value now and the new value, a new issue as its title and fields, a
 * message as whom it reaches with its text, a change of a document's text as its sentences and
 * its diff, an edit or a delete of a comment or an issue message as the post's author and issue
 * with its body (`PostBody`), and any other change as what it does. A row that closes, deletes or
 * removes something is marked in red; a row whose item changed after the person saw it is marked
 * in amber, says its value now, and waits to be ticked again; a row unticked because a row it
 * needs is says so. A row whose post was deleted after the plan read it has no tick, as nothing
 * edits or deletes a deleted post, and the rows that need it are left unticked
 * (`untickedRows`). In Auto-review it says it passed review, or why it waits. Once its plan no
 * longer waits (design v2, state 7) it ticks in place - a ring while it waits its turn, a spinner
 * while it runs, a check once it ran - and says how it stands (`rowOutcome`): a message's result,
 * a failure's reason with "Say what to do instead" (`onFix`, where given), "Not run: it needs the
 * change above". Once its run was undone, it says what became of its change (`undo`): a change
 * Undo took back whole reads reverted - the revert arrow in place of its check, its change drawn
 * as the plan card drew it, plain "old → new" with no strike, and "Reverted" under it (review
 * N3) - while a change that stayed keeps its check and says why.
 */
export function PlanRowView({
  draft,
  onFix,
  plan,
  row,
  undo,
}: {
  readonly draft: PlanDraft;
  readonly onFix?: () => void;
  readonly plan: GoingPlanAnswer;
  readonly row: GoingPlanRow;
  readonly undo?: GoingUndoLine;
}): ReactNode {
  const agents = useAgentText();
  const waiting = rowWaits(row) && planWaits(plan);
  const unticked = untickedRows(plan, draft);
  const ticked = !unticked.has(row.row);
  // A row whose post is deleted waits with no tick: nothing edits or deletes a deleted post.
  const tickable = waiting && !postGone(row);
  const edits = draft.edits.get(row.row);
  const newIssues = newIssueTitles(plan.rows ?? [], draft);
  const args = argumentsOf(row);
  const kind = rowKind(row);
  const changes = rowChanges(row, edits, newIssues);
  const removes = rowRemoves(row, changes);
  const changed = row.changed && tickable;
  const session = textOf(args.session);
  // A document row names what it does from its own data (`documentNaming`), and its tick is named
  // so as well.
  const naming = kind === "document" ? documentNaming(row) : undefined;
  const label =
    naming === undefined
      ? rowLabel(
          row,
          session === "" ? edits : { ...edits, session: agents.sessionText(session, undefined) },
          newIssues
        )
      : naming.issue === ""
        ? naming.words
        : `${naming.words} ${naming.issue}`;
  const review = reviewWords(plan, row);
  // A row says how it stands once its plan no longer waits, and where it failed while the plan
  // waits (a row Auto-review ran); a discarded plan's rows ran nothing and say nothing of it.
  // Once Undo answered, the row's one status line is what became of its change (`undo`): a sent
  // message reads "Sent, not undone" alone, not "Sent" above it.
  const outcome =
    undo === undefined &&
    review === undefined &&
    plan.state !== "rejected" &&
    (plan.state !== "waiting" || row.state === "failed")
      ? rowOutcome(row)
      : undefined;
  const resultMessage = outcome === undefined && undo === undefined ? rowResultMessage(row) : "";
  // A change Undo took back whole reads reverted, with no check; one that stayed keeps its check
  // and its line says why.
  const reverted = undo !== undefined && wasReverted(undo);
  const mark: RowMark =
    plan.state === "rejected" ? "not_run" : reverted ? "reverted" : ROW_MARKS[row.state];
  const item = row.item || row.issue;
  const project = textOf(args.project) || issueProject(item);
  // A message on an issue's conversation that was sent says where it went (`SentOnIssue`).
  const postedOn =
    row.tool === "dispatch_message" && row.result?.delivery === "sent"
      ? item || textOf(args.issue)
      : "";
  const field = (change: RowChange, shape: "line" | "title" | "part" | "diff") => (
    <FieldChange
      change={change}
      editable={tickable}
      issueKey={kind === "new_issue" ? (newIssues.get(row.row) ?? "") : item}
      key={`${plan.run}:${plan.plan}:${change.field}`}
      newIssues={newIssues}
      onEdit={(value) => setDraft(editedDraft(draft, row.row, change.field, value))}
      project={project}
      shape={shape}
    />
  );
  const document = documentChange(row, plan.summary);
  let body: ReactNode;
  if (kind === "new_issue") {
    body = <NewIssueBody changes={changes} field={field} spec={textOf(args.spec)} />;
  } else if (kind === "message") {
    body = <MessageBody row={row} waiting={waiting} />;
  } else if (kind === "post") {
    body = (
      <PostBody
        changes={changes}
        field={(change) => field(change, "diff")}
        row={row}
        waiting={tickable}
      />
    );
  } else if (kind === "document" && document !== undefined && naming !== undefined) {
    body = <DocumentBody document={document} naming={naming} />;
  } else if (kind === "change" || kind === "claim") {
    // The chip, then each field it changes under it, in the bar and in the panel alike: the
    // fields start at one edge in every row, whatever the width of the chip above them.
    body = (
      <div className="space-y-1">
        <ItemSlot item={item} />
        <div className="space-y-1">{changes.map((change) => field(change, "line"))}</div>
      </div>
    );
  } else {
    const posts = rowText(row);
    body = (
      <div className="space-y-1">
        <p className={textPrimaryOnSurface}>{label}</p>
        {changes.map((change) => field(change, "line"))}
        {posts === "" ? null : <Quote text={posts} />}
      </div>
    );
  }
  // What a row takes out is marked in red until Undo puts it back, and while it can still run: a
  // row whose post was deleted since takes nothing out. The mark is a bar in a gutter every row
  // keeps, so a marked row's tick stands in the same column as every other row's.
  const removing = removes && !reverted && !deletedSince(row);
  const removal = removing ? removalWords(changes) : "";
  const gutter = removing
    ? calloutDangerBorder
    : changed
      ? calloutWarningBorder
      : borderTransparent;
  // A new issue's title carries its Edit while the row waits, a button as tall as the line (44 px
  // on a phone, 28 px from `sm`), so the tick stands lower to sit level with that line's words, as
  // every other row's tick sits level with its first line (review a4).
  const tickTop = kind === "new_issue" && tickable ? "mt-3.5 sm:mt-1.5" : "mt-0.5";
  return (
    <li
      className={`border-t py-2 outline-none first:border-t-0 focus-visible:ring-2 ${focusVisibleRing} ${borderDefault}`}
      data-changed={changed ? "true" : undefined}
      data-kind={kind}
      data-removes={removes ? "true" : undefined}
      data-row={row.row}
      data-state={row.state}
      data-testid="going-plan-row"
      data-ticked={waiting ? String(ticked) : undefined}
      tabIndex={-1}
    >
      <div className="flex items-start gap-2">
        <span
          aria-hidden="true"
          className={`-ml-1 w-0 self-stretch border-l-4 ${gutter}`}
          data-testid="going-plan-row-gutter"
        />
        {tickable ? (
          <input
            aria-label={label}
            checked={ticked}
            className={`${tickTop} size-4 shrink-0 cursor-pointer`}
            onChange={() => setDraft(toggledDraft(draft, plan.rows ?? [], row.row))}
            type="checkbox"
          />
        ) : waiting ? (
          // The tick's room, so the row's words start where every ticked row's do.
          <span aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
        ) : mark === "waits" ? null : (
          <span
            className="mt-0.5 inline-flex size-4 shrink-0 items-center justify-center text-sm"
            data-mark={mark}
            data-testid="going-plan-row-mark"
          >
            <span aria-hidden="true">{MARK_GLYPHS[mark]}</span>
            <span className="sr-only">{MARK_WORDS[mark]}</span>
          </span>
        )}
        <div className="min-w-0 flex-1 space-y-1 text-sm">
          {body}
          {removal === "" ? null : (
            <p className={`text-xs font-medium ${dangerText}`} data-testid="going-plan-removes">
              {removal}
            </p>
          )}
          {changed ? (
            <p className={`text-xs ${inlineWarningText}`} data-testid="going-plan-changed">
              Changed since the plan: now {valuesNowWords(changes)}
            </p>
          ) : null}
          {tickable && needsUnticked(unticked, row) ? (
            <p className={`text-xs ${textMutedOnSurface}`} data-testid="going-plan-needs">
              Needs the change above
            </p>
          ) : null}
          {review === undefined ? null : (
            <p className={`text-xs ${textMutedOnSurface}`} data-testid="going-plan-row-state">
              {review}
            </p>
          )}
          {outcome === undefined ? null : (
            <p
              className={`text-xs ${outcome.failed ? dangerText : textMutedOnSurface}`}
              data-testid="going-plan-row-state"
            >
              {postedOn === "" ? outcome.text : <SentOnIssue issue={postedOn} row={row} />}
              {outcome.failed && onFix !== undefined ? (
                <>
                  {" "}
                  <button
                    className={`underline ${linkText} ${linkHoverText}`}
                    data-testid="going-plan-fix"
                    onClick={onFix}
                    type="button"
                  >
                    Say what to do instead
                  </button>
                </>
              ) : null}
            </p>
          )}
          {resultMessage === "" ? null : (
            <p className={`text-xs ${row.state === "failed" ? dangerText : textMutedOnSurface}`}>
              {resultMessage}
            </p>
          )}
          {undo === undefined ? null : (
            <p className={`text-xs ${textSecondaryOnSurface}`} data-testid="going-undo-line">
              <UndoResult line={undo} />
            </p>
          )}
        </div>
      </div>
    </li>
  );
}

/**
 * The plan's one primary button, "Approve 3 changes", which counts the rows that wait and are
 * ticked - those the page draws too - with ↵ beside it where Enter in the box approves; Discard,
 * which rejects the plan; and why Dispatch refused the last answer. Approve is off where Enter
 * approves nothing (`approvable`): with no row ticked, and while an answer to the plan is out.
 */
export function PlanControls({
  answering,
  draft,
  enterApproves,
}: {
  readonly answering: PlanAnswering;
  readonly draft: PlanDraft;
  readonly enterApproves: boolean;
}): ReactNode {
  const plan = answering.plan;
  if (plan === undefined) {
    return null;
  }
  const count = tickedCount(plan, draft);
  return (
    <div className="space-y-1">
      {planWaits(plan) ? (
        <div className="flex flex-wrap items-center gap-2">
          <button
            className={primaryButton}
            data-testid="going-plan-approve"
            disabled={!approvable(plan, draft, answering.pending)}
            onClick={answering.approve}
            type="button"
          >
            {count === 1 ? "Approve 1 change" : `Approve ${count} changes`}
            {enterApproves ? (
              <span aria-hidden="true" className="ml-2 font-normal">
                ↵
              </span>
            ) : null}
          </button>
          <button
            className={secondaryButtonPaired}
            data-testid="going-plan-reject"
            disabled={answering.pending}
            onClick={answering.reject}
            type="button"
          >
            Discard
          </button>
        </div>
      ) : null}
      {answering.refusal === undefined ? null : (
        <p className={`text-xs ${dangerText}`} data-testid="going-plan-refused" role="alert">
          {answering.refusal}
        </p>
      )}
    </div>
  );
}

/** The approval mode the plan's run is in, as a quiet line that says what it is - "Approval:
 *  Ask ›" or "Approval: Auto-review ›" - and links to where the person sets it: a lone "Ask" read
 *  as an action. It never breaks: in a narrow panel it goes under the title whole. */
function ModeLink({ mode }: { readonly mode: GoingPlanAnswer["mode"] }): ReactNode {
  const routed = useInRouterContext();
  const className = `shrink-0 text-xs whitespace-nowrap hover:underline ${textMutedOnSurface}`;
  const title = "Set which changes wait for you, in Settings";
  const words = (
    <>
      Approval: {MODE_WORDS[mode]} <span aria-hidden="true">›</span>
    </>
  );
  return routed ? (
    <Link className={className} data-testid="going-plan-mode" title={title} to={MODE_SETTING_PATH}>
      {words}
    </Link>
  ) : (
    <a className={className} data-testid="going-plan-mode" href={MODE_SETTING_PATH} title={title}>
      {words}
    </a>
  );
}

/**
 * When this card first drew `plan` with the rows sent to run all come to an end (its state
 * "done"), in this page's clock: the time its last row ran, as this page saw it. The run may work
 * on after that, to write its answer, so its end is later. Undefined until then.
 */
function useRanAt(plan: GoingPlanAnswer | undefined): number | undefined {
  const ran = plan?.state === "done" ? `${plan.run}:${plan.plan}` : undefined;
  const [seen, setSeen] = useState<{ readonly plan: string; readonly at: number }>();
  if (ran !== undefined && seen?.plan !== ran) {
    setSeen({ at: Date.now(), plan: ran });
  }
  return seen !== undefined && seen.plan === ran ? seen.at : undefined;
}

/** The soft shadow the pinned Approve bar casts up over the rows while some stand under it. */
const COVERING_SHADOW = "shadow-[0_-6px_12px_-6px_rgba(15,23,42,0.18)]";

/** The scroll that holds `element`: its nearest ancestor whose content scrolls (`overflow-y` auto
 *  or scroll), the conversation of the bar or of the panel; undefined where none does. */
function scrollerOf(element: HTMLElement): HTMLElement | undefined {
  for (let at = element.parentElement; at !== null; at = at.parentElement) {
    const { overflowY } = getComputedStyle(at);
    if (overflowY === "auto" || overflowY === "scroll") return at;
  }
  return undefined;
}

/** Scrolls the conversation that holds `row` until the row's top stands at the conversation's
 *  top edge, and moves nothing else. `scrollIntoView` scrolls every ancestor that can scroll, and
 *  it scrolled the bar's frame under its header and its box (review B1). */
function scrollToRow(row: HTMLElement): void {
  const scroller = scrollerOf(row);
  if (scroller === undefined) return;
  scroller.scrollTop += row.getBoundingClientRect().top - scroller.getBoundingClientRect().top;
}

/**
 * The rows of a waiting plan's card that its pinned Approve and Discard (`bar`) cover, by their
 * numbers, read again on every scroll, resize and change of the list's size (review A1): each row
 * any of whose words stand below the bar's top edge. Its bottom padding may stand there: a row
 * whose words all show is read. None while `watching` is false.
 */
function useHiddenRows(
  list: RefObject<HTMLUListElement | null>,
  bar: RefObject<HTMLDivElement | null>,
  watching: boolean
): readonly number[] {
  const [hidden, setHidden] = useState<readonly number[]>([]);
  useLayoutEffect(() => {
    if (!watching) {
      setHidden((was) => (was.length === 0 ? was : []));
      return undefined;
    }
    const measure = () => {
      const edge = bar.current?.getBoundingClientRect().top;
      const under =
        edge === undefined
          ? []
          : [...(list.current?.querySelectorAll<HTMLElement>(ROW) ?? [])]
              .filter((row) => {
                const padding = Number.parseFloat(getComputedStyle(row).paddingBottom) || 0;
                return row.getBoundingClientRect().bottom - padding > edge + 1;
              })
              .map((row) => Number(row.dataset.row));
      setHidden((was) =>
        was.length === under.length && was.every((row, at) => row === under[at]) ? was : under
      );
    };
    measure();
    document.addEventListener("scroll", measure, { capture: true, passive: true });
    window.addEventListener("resize", measure);
    const sizes = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(measure);
    if (list.current !== null) sizes?.observe(list.current);
    return () => {
      document.removeEventListener("scroll", measure, { capture: true });
      window.removeEventListener("resize", measure);
      sizes?.disconnect();
    };
  }, [bar, list, watching]);
  return hidden;
}

/**
 * The plan card of the bar and the side panel. While its plan waits (design v3, state 6): a
 * title made from the plan's rows and its approval mode, linked to its setting; the rows no page
 * on the screen draws, with "2 changes show on this page" for the rest; and the plan's Approve and
 * Discard, pinned at the foot of the view while the card is taller than it. While rows stand under
 * that bar (review A1), its top edge casts a soft shadow and it says how many and names those of
 * them that cannot be undone - "3 more ↓ · includes a message that cannot be undone" - its "3
 * more ↓" a button that scrolls the conversation to them, so Enter never approves a row the
 * person was not shown was there. A row the page draws
 * (`PlannedChanges`) shows there and not here, so the rows of one plan can show in both places,
 * each once, and Approve approves the ticked rows of both. While the plan waits, the box's ↑
 * moves the focus onto the card's rows (`usePlanCardKeys`).
 *
 * Once approved (design v2, state 7), its title follows its rows, "Applying 1 of 3…" then
 * "Applied 3 changes · 16:42", the time its last row ran (`useRanAt`) or its run ended
 * (`appliedAt`), whichever this page knows first, each row ticks in place, and a
 * failed row's "Say what to do instead" puts the cursor in the box. While Dispatch's record of
 * the run says Undo can take a change of it back (`undoRecord`), the card keeps one Undo (R6),
 * which says how many its press takes back, "Undo 2 changes", as that record counts them: a
 * message stays sent and a setting stays set, so neither counts, and a plan of those alone offers
 * none. Once pressed, its title reads "Undone · 3 changes reverted" and each row says what became
 * of its change, in one line. An old turn's card (`folded`, design v2, state 8) draws a plan that
 * no longer waits as its one line, "Applied 3 changes · Undo 3 changes", which opens it. A run
 * Dispatch says was undone before this page pressed its Undo reads "Undone · 11:35 AM" where no
 * change of it stays, and "Partly undone · 11:35 AM" where one does, with Undo for what it can
 * still take back. A plan that could not be read says so, with Retry.
 */
export function PlanCard({
  answering,
  appliedAt,
  folded = false,
}: {
  readonly answering: PlanAnswering;
  readonly appliedAt?: number;
  readonly folded?: boolean;
}): ReactNode {
  const going = useGoing();
  const plan = answering.plan;
  const draft = usePlanDraft(plan);
  const undoing = useRunUndo(plan?.run ?? "");
  const ranAt = useRanAt(plan);
  const [open, setOpen] = useState(false);
  const list = useRef<HTMLUListElement>(null);
  const actions = useRef<HTMLDivElement>(null);
  const waiting = plan !== undefined && planWaits(plan);
  const hidden = useHiddenRows(list, actions, waiting);
  useEffect(() => {
    if (!waiting) return undefined;
    return holdCardFocus(() => {
      const rows = list.current?.querySelectorAll<HTMLElement>(ROW);
      const nearest = rows?.[rows.length - 1];
      nearest?.focus();
      return nearest !== undefined;
    });
  }, [waiting]);
  if (plan === undefined || draft === undefined) {
    const unreadable = answering.unreadable;
    return unreadable === undefined ? null : (
      <section
        aria-label="Plan"
        className={`rounded-lg border p-3 ${borderDefault}`}
        data-testid="going-plan-unreadable"
      >
        <QueryError
          message={unreadable.message}
          onRetry={unreadable.retry}
          retrying={unreadable.retrying}
        />
      </section>
    );
  }
  const lines = undoing.lines;
  const shut = folded && !waiting && !open;
  // The time its last row ran, or its run ended, whichever this page knows to be earlier.
  const appliedTime =
    appliedAt === undefined || ranAt === undefined
      ? (appliedAt ?? ranAt)
      : Math.min(appliedAt, ranAt);
  // What Dispatch's record of the run says its Undo did and can still do: the title of a run
  // undone where this page holds no answer of a press, and the Undo the card offers.
  const record = going.runs.get(plan.run)?.record;
  const recorded = record === undefined ? undefined : undoRecord(record);
  const title = cardTitle(
    plan,
    lines,
    folded ? undefined : appliedTime,
    recorded?.at === undefined ? undefined : { at: recorded.at, stays: recorded.stays }
  );
  // The rows the page draws are the open plan's, which an old turn's card is not.
  const onPage = folded ? new Set<number>() : new Set([...going.onPage.values()].flat());
  const rows = (plan.rows ?? []).filter((row) => !onPage.has(row.row));
  // A line of a change no row drawn here names - one the page drew - still says what became of it.
  const unmatched = (lines ?? []).filter(
    (line) => line.plan !== plan.plan || !rows.some((row) => row.row === line.row)
  );
  // The rows the pinned bar covers (`useHiddenRows`), and those of them Approve would run that
  // nothing takes back once they run, named (`irreversibleWords`).
  const covered = rows.filter((row) => hidden.includes(row.row));
  const unticked = untickedRows(plan, draft);
  const final = irreversibleWords(covered.filter((row) => rowWaits(row) && !unticked.has(row.row)));
  // Undo is offered for what the record says it can still take back, and names that count; an
  // Undo pressed on this page has answered, and its rows say what became of each change.
  const undoable = recorded?.undoable ?? 0;
  const offered = lines === undefined && undoable > 0;
  const label = undoButtonWords(undoable);
  const undo = offered ? (
    <button
      className={secondaryButtonCompact}
      data-testid="going-undo-button"
      disabled={undoing.pending}
      onClick={undoing.undo}
      type="button"
    >
      {label}
    </button>
  ) : null;
  const refusal =
    undoing.refusal === undefined ? null : (
      <p className={`text-xs ${dangerText}`} role="alert">
        {undoing.refusal}
      </p>
    );
  if (shut) {
    return (
      <section
        aria-label="Plan"
        className="space-y-1"
        data-folded="true"
        data-plan={plan.plan}
        data-state={plan.state}
        data-testid="going-plan"
      >
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
          <button
            aria-expanded={false}
            className={`flex min-h-6 items-center gap-1 text-left text-xs hover:underline ${textSecondaryOnSurface}`}
            data-testid="going-plan-title"
            onClick={() => setOpen(true)}
            type="button"
          >
            <span>{title}</span>
            <span aria-hidden="true">›</span>
          </button>
          {undo === null ? null : (
            <>
              <span aria-hidden="true" className={`text-xs ${textMutedOnSurface}`}>
                ·
              </span>
              {undo}
            </>
          )}
        </div>
        {refusal}
      </section>
    );
  }
  return (
    <section
      aria-label="Plan"
      className={`space-y-2 rounded-lg border p-3 ${borderDefault}`}
      data-plan={plan.plan}
      data-state={plan.state}
      data-testid="going-plan"
    >
      <header className="flex items-baseline justify-between gap-3">
        <h3
          aria-live="polite"
          className={`min-w-0 flex-1 text-sm font-semibold ${textPrimaryOnSurface}`}
          data-testid="going-plan-title"
        >
          {folded && !waiting ? (
            <button
              aria-expanded
              className="text-left hover:underline"
              onClick={() => setOpen(false)}
              type="button"
            >
              {title}
            </button>
          ) : (
            title
          )}
        </h3>
        {waiting ? <ModeLink mode={plan.mode} /> : null}
      </header>
      {plan.summary === "" ? null : (
        <p className={`text-sm ${textSecondaryOnSurface}`}>
          <TimedWords text={plan.summary} />
        </p>
      )}
      {rows.length === 0 ? null : (
        // The rows take the card's keys (`onRowKey`): the focus reaches them from the box.
        <ul data-testid="going-plan-card-rows" onKeyDown={onRowKey} ref={list}>
          {rows.map((row) => (
            <PlanRowView
              draft={draft}
              key={row.row}
              onFix={folded ? undefined : focusConversationBox}
              plan={plan}
              row={row}
              undo={lines?.find((line) => line.plan === plan.plan && line.row === row.row)}
            />
          ))}
        </ul>
      )}
      {onPage.size > 0 ? (
        <p className={`text-xs ${textMutedOnSurface}`} data-testid="going-plan-on-page-note">
          {onPage.size === 1 ? "1 change shows" : `${onPage.size} changes show`} on this page
        </p>
      ) : null}
      {unmatched.length === 0 ? null : (
        <ul className={`text-xs ${textSecondaryOnSurface}`}>
          {unmatched.map((line) => (
            <li data-testid="going-undo-line" key={`${line.plan}:${line.row}:${line.tool}`}>
              <UndoResult line={line} />
            </li>
          ))}
        </ul>
      )}
      {waiting ? null : <PlanControls answering={answering} draft={draft} enterApproves />}
      {undo}
      {refusal}
      {waiting ? (
        // Approve and Discard stay at the foot of the view while the card is taller than it
        // (design v3, state 6): the card shows from its question down, and its one primary
        // action never sits below the fold. The bar is pinned to the bottom edge of the
        // conversation's scroll, which keeps no padding of its own there (`ConversationView`), so
        // no row shows under it; its fill and its top rule cover the rows that scroll behind it.
        // It is the card's last part, so once the card's end is in view it stands there.
        <div
          className={`sticky bottom-0 z-10 -mx-3 -mb-3 rounded-b-lg border-t px-3 py-3 ${borderDefault} ${surfaceBg} ${covered.length === 0 ? "" : COVERING_SHADOW}`}
          data-testid="going-plan-actions"
          ref={actions}
        >
          {covered.length === 0 ? null : (
            // The line is an action, in the link's blue with a ring on focus, and it names each
            // row under the bar that nothing takes back once Approve runs it (review A1).
            <p className={`mb-2 text-xs ${textMutedOnSurface}`} data-testid="going-plan-more">
              <button
                className={`rounded font-medium outline-none hover:underline focus-visible:ring-2 ${focusVisibleRing} ${linkText} ${linkHoverText}`}
                data-testid="going-plan-more-button"
                onClick={() => {
                  const first = list.current?.querySelector<HTMLElement>(
                    `${ROW}[data-row="${covered[0]?.row}"]`
                  );
                  if (first !== null && first !== undefined) scrollToRow(first);
                }}
                type="button"
              >
                {covered.length} more <span aria-hidden="true">↓</span>
              </button>
              {final === "" ? null : (
                // "↓" keeps room on its right that the words keep none of on their left, about
                // 4 px in the shots, so the words keep it as padding and the "·" stands as far
                // from the arrow as from them (review m4, as N5 did for the notice).
                <>
                  {" · "}
                  <span className="pl-1">includes {final} that cannot be undone</span>
                </>
              )}
            </p>
          )}
          <PlanControls answering={answering} draft={draft} enterApproves />
        </div>
      ) : null}
    </section>
  );
}
