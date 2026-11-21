import { type ReactNode, useMemo, useState } from "react";

import type { GoingPlanAnswer, GoingPlanRow } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  dangerText,
  diffAddedBg,
  diffAddedText,
  diffRemovedBg,
  diffRemovedText,
  inputClasses,
  primaryButton,
  secondaryButtonCompact,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { changedWords } from "../doc/change-set";
import { draftOf, type PlanDraft, setDraft, useGoing } from "./going-store";
import {
  type ChangePiece,
  documentChange,
  editedDraft,
  makesIssue,
  rowChanges,
  rowLabel,
  rowWaits,
  toggledDraft,
  valueText,
} from "./plan-rows";
import type { PlanAnswering } from "./usePlan";

/** How a row stands, in the person's words. */
const ROW_STATES: Readonly<Record<GoingPlanRow["state"], string>> = {
  approved: "Approved",
  done: "Done",
  failed: "Failed",
  running: "Running",
  skipped: "Skipped: a change it needs did not run",
  unticked: "Not run",
  waiting: "Waits for you",
};

/** One piece of a document change, its words out and in marked as a diff marks them. A whole
 *  document's pieces are diffed within the bound `changedWords` keeps, and once for each pair. */
function PieceDiff({ piece }: { readonly piece: ChangePiece }): ReactNode {
  const { added, removed } = piece;
  const parts = useMemo(() => changedWords(removed, added), [removed, added]);
  return (
    <p
      className="whitespace-pre-wrap break-words font-mono text-xs"
      data-testid="going-plan-diff-piece"
    >
      {parts.map((part, index) => {
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
      })}
    </p>
  );
}

/** One field a row writes: its name, the value now and the new value, which the person can edit
 *  where it is text and the row still waits. Its edit box starts from the new value of the plan it
 *  is drawn for, and its caller keys it to that plan, so a revised plan or a new run's never opens
 *  with an earlier plan's value or the person's unsaved typing. */
function FieldChange({
  editable,
  field,
  next,
  now,
  onEdit,
  showsNow,
}: {
  readonly editable: boolean;
  readonly field: string;
  readonly next: unknown;
  readonly now: unknown;
  readonly onEdit: (value: string) => void;
  readonly showsNow: boolean;
}): ReactNode {
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState(typeof next === "string" ? next : "");
  return (
    <div
      className="flex flex-wrap items-baseline gap-x-2 text-xs"
      data-field={field}
      data-testid="going-plan-field"
    >
      <span className={`font-medium ${textSecondaryOnSurface}`}>{field}</span>
      {showsNow ? (
        <>
          <span className={textMutedOnSurface} data-testid="going-plan-now">
            {valueText(now)}
          </span>
          <span aria-hidden="true">→</span>
        </>
      ) : null}
      {editing ? (
        <form
          className="flex items-center gap-1"
          onSubmit={(event) => {
            event.preventDefault();
            onEdit(text);
            setEditing(false);
          }}
        >
          <input
            aria-label={`New ${field}`}
            className={`rounded border px-1 py-0.5 text-xs ${inputClasses(true)}`}
            onChange={(event) => setText(event.target.value)}
            onKeyDown={(event) => {
              // Enter keeps the edit, and the plan is approved from the box alone.
              event.stopPropagation();
              if (event.key === "Escape") setEditing(false);
            }}
            value={text}
          />
          <button className={secondaryButtonCompact} type="submit">
            Keep
          </button>
        </form>
      ) : (
        <>
          <span className={textPrimaryOnSurface} data-testid="going-plan-next">
            {valueText(next)}
          </span>
          {editable ? (
            <button
              aria-label={`Edit ${field}`}
              className={secondaryButtonCompact}
              onClick={() => setEditing(true)}
              type="button"
            >
              Edit
            </button>
          ) : null}
        </>
      )}
    </div>
  );
}

/**
 * One row of a plan, wherever it is drawn: its tick, the change (the tool, and the item it
 * changes), each field it writes with the value now and the new value, and for a change of a
 * document's text the sentences that say what it changes with its diff under them, closed. It
 * says where it stands, why it waits in every mode, and that its item changed after the person saw
 * it, which shows the new value and waits again.
 */
export function PlanRowView({
  draft,
  plan,
  row,
}: {
  readonly draft: PlanDraft;
  readonly plan: GoingPlanAnswer;
  readonly row: GoingPlanRow;
}): ReactNode {
  const waiting = rowWaits(row) && plan.state === "waiting";
  const ticked = !draft.unticked.has(row.row);
  const edits = draft.edits.get(row.row);
  const document = documentChange(row, plan.summary);
  const changes = rowChanges(row, edits);
  const rows = plan.rows ?? [];
  const label = rowLabel(row);
  return (
    <li
      className={`space-y-1 border-t py-2 first:border-t-0 ${borderDefault}`}
      data-row={row.row}
      data-state={row.state}
      data-testid="going-plan-row"
      data-ticked={waiting ? String(ticked) : undefined}
    >
      <div className="flex items-center gap-2 text-sm">
        {waiting ? (
          <input
            aria-label={`Run ${label}`}
            checked={ticked}
            className="size-4 cursor-pointer"
            onChange={() => setDraft(toggledDraft(draft, rows, row.row))}
            type="checkbox"
          />
        ) : null}
        <span className={`font-medium ${textPrimaryOnSurface}`}>{label}</span>
        <span className={`text-xs ${textMutedOnSurface}`} data-testid="going-plan-row-state">
          {ROW_STATES[row.state]}
        </span>
      </div>
      {document === undefined ? (
        changes.map((change) => (
          <FieldChange
            editable={waiting && change.editable}
            field={change.field}
            key={`${plan.run}:${plan.plan}:${change.field}`}
            next={change.next}
            now={change.now}
            onEdit={(value) => setDraft(editedDraft(draft, row.row, change.field, value))}
            showsNow={!makesIssue(row)}
          />
        ))
      ) : (
        <div className="space-y-1">
          <p className={`text-sm ${textSecondaryOnSurface}`} data-testid="going-plan-sentences">
            {document.sentences}
          </p>
          <details data-testid="going-plan-diff">
            <summary className={`cursor-pointer text-xs ${textMutedOnSurface}`}>The diff</summary>
            {document.pieces.map((piece, index) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: a piece is its place in the change, which never changes for one row
              <PieceDiff key={index} piece={piece} />
            ))}
          </details>
        </div>
      )}
      {row.changed ? (
        <p className={`text-xs ${dangerText}`} data-testid="going-plan-changed">
          Its item changed after you saw it: this is its value now, and it waits for you again.
        </p>
      ) : null}
      {(row.waits ?? []).length > 0 && waiting ? (
        <p className={`text-xs ${textMutedOnSurface}`}>
          Waits for you: {(row.waits ?? []).join("; ")}
        </p>
      ) : null}
      {row.result !== null && row.result.message !== "" ? (
        <p className={`text-xs ${row.state === "failed" ? dangerText : textMutedOnSurface}`}>
          {row.result.message}
        </p>
      ) : null}
    </li>
  );
}

/** The plan's Approve and Reject, and why Dispatch refused the last answer. */
export function PlanControls({ answering }: { readonly answering: PlanAnswering }): ReactNode {
  const plan = answering.plan;
  if (plan === undefined) {
    return null;
  }
  return (
    <div className="space-y-1">
      {plan.state === "waiting" ? (
        <div className="flex flex-wrap items-center gap-2">
          <button
            className={primaryButton}
            data-testid="going-plan-approve"
            disabled={answering.pending}
            onClick={answering.approve}
            type="button"
          >
            Approve
          </button>
          <button
            className={secondaryButtonCompact}
            data-testid="going-plan-reject"
            disabled={answering.pending}
            onClick={answering.reject}
            type="button"
          >
            Reject
          </button>
          <span className={`text-xs ${textMutedOnSurface}`}>
            Nothing changes until you approve. Enter approves; words say what to change.
          </span>
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

/**
 * The plan card of the bar and the side panel: the rows no page on the screen draws, and the
 * plan's Approve and Reject. A row the page draws (`PlannedChanges`) shows there and not here, so
 * the rows of one plan can show in both places, each once. A plan that could not be read says
 * so, with Retry.
 */
export function PlanCard({ answering }: { readonly answering: PlanAnswering }): ReactNode {
  const going = useGoing();
  const plan = answering.plan;
  if (plan === undefined) {
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
  const onPage = new Set([...going.onPage.values()].flat());
  const rows = (plan.rows ?? []).filter((row) => !onPage.has(row.row));
  const draft = draftOf(plan.run, plan.plan, going.draft);
  return (
    <section
      aria-label="Plan"
      className={`space-y-2 rounded-lg border p-3 ${borderDefault}`}
      data-plan={plan.plan}
      data-state={plan.state}
      data-testid="going-plan"
    >
      {plan.summary === "" ? null : (
        <p className={`text-sm ${textPrimaryOnSurface}`}>{plan.summary}</p>
      )}
      {rows.length === 0 ? null : (
        <ul data-testid="going-plan-card-rows">
          {rows.map((row) => (
            <PlanRowView draft={draft} key={row.row} plan={plan} row={row} />
          ))}
        </ul>
      )}
      {onPage.size > 0 ? (
        <p className={`text-xs ${textMutedOnSurface}`} data-testid="going-plan-on-page-note">
          {onPage.size === 1 ? "1 change is" : `${onPage.size} changes are`} drawn on the page.
        </p>
      ) : null}
      <PlanControls answering={answering} />
    </section>
  );
}
