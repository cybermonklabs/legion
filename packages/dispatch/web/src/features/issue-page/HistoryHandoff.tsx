import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useId, useMemo, useRef, useState } from "react";

import { api } from "../../api/client";
import { useFocusHandOff } from "../../hooks/useFocusHandOff";
import {
  pageInkText,
  pageMarkBg,
  pageMutedText,
  pageQuietText,
  pageRowAct,
} from "../../theme/classes";
import { type HandoffField, handoffFields } from "../conversation/conversation-model";
import { standsFailed } from "../refs/reference-target";
import { DetailsErrorLine, retryRead, useRetryHandOff } from "./details-error";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS, sentenceCase } from "./page-words";

/** How a handoff shows JSON: an array or object field, or a body that is not a JSON object, in
 *  the muted grey on the mark grey, as a node's body draws a check's command. */
const handoffPre = `mt-1 max-h-64 overflow-auto rounded-md p-2 text-xs whitespace-pre-wrap ${pageMarkBg} ${pageMutedText}`;

/** The page's names of the fields every Legion handoff record carries. */
const KNOWN_FIELDS: Readonly<Record<string, string>> = CONTROL_LABELS.handoff_fields;

/** One row of a handoff's opened fields: `handoffFields`' row, and which entry of the `proof`
 *  list it is, counted from 1, where it is one. */
interface FieldRow extends HandoffField {
  readonly entry: number | undefined;
}

/**
 * The fields of a handoff's line in the issue's history, which took over the conversation's
 * handoff cards: drawn open under the line - each top-level field of the version's body a
 * labelled row, the `proof` list one row per entry, as the old conversation's handoff card drew
 * it - with no press. Only the reader folds them, by Hide fields on the handoff's line, and Show
 * fields opens them again. The body is read as the handoff's line is drawn, and a version never
 * changes once written, so the read is kept for the page's life: it is never stale, and folding
 * the fields never lets it go.
 */
export function HistoryHandoff({ handoffId }: { handoffId: string }): ReactNode {
  const [expanded, setExpanded] = useState(true);
  const fieldsId = useId();
  return (
    <span className="basis-full">
      <button
        aria-controls={expanded ? fieldsId : undefined}
        aria-expanded={expanded}
        className={`dispatch-inline-link text-xs font-medium ${pageMutedText} ${pageRowAct}`}
        onClick={() => setExpanded((open) => !open)}
        type="button"
        {...pageChrome}
      >
        {expanded ? CONTROL_LABELS.hide_fields : CONTROL_LABELS.show_fields}
      </button>
      {expanded ? <HandoffFields handoffId={handoffId} id={fieldsId} /> : null}
    </span>
  );
}

/** A handoff's opened fields, or the line that says they are being read, or that the read
 *  failed; `id` names whichever of them stands in the fields' place. */
function HandoffFields({ handoffId, id }: { handoffId: string; id: string }): ReactNode {
  const handoff = useQuery({
    gcTime: Number.POSITIVE_INFINITY,
    queryKey: ["handoff", handoffId],
    queryFn: () => api.getHandoff(handoffId),
    staleTime: Number.POSITIVE_INFINITY,
  });
  const failure = useRef<HTMLSpanElement>(null);
  // A focus on a line that goes - the Retry of the failure line once the read the reader's press
  // of it sent lands, or the loading line - goes on to what the reader's press asked for, where no
  // Enter undoes it: the Retry of a failure line drawn in its place, else what stands in the
  // fields' place, the fields once the read has landed. The loading line takes it from the Retry
  // only after a reset of the query, which takes the failure line away while the read is out.
  // The toggle above them would not do: a second press of the Retry's key - a key that repeated,
  // or a reader who saw nothing happen and pressed again - would land on it and shut the fields.
  // The Retry hands it on only after the reader's press of it (`useRetryHandOff`): a line a read
  // that answers on its own takes away - a window focus, an invalidation after a write - leaves
  // the focus on the page's body, as on every page of Dispatch. The loading line, which no Tab
  // reaches, holds the focus only where the reader put it there - their press on it, or their
  // Retry's hand-on after such a reset - and goes only as the read it stands for answers, so it
  // hands the focus on with no further press.
  const handOffTo = () =>
    failure.current?.querySelector<HTMLElement>("button") ?? document.getElementById(id);
  const retry = useRetryHandOff(handOffTo);
  const loadingLeft = useFocusHandOff(handOffTo);
  const fields = useMemo(
    () => (handoff.data === undefined ? undefined : fieldRows(handoff.data.body)),
    [handoff.data]
  );
  // The failure line stands in the fields' place, and in place of the loading line, while the
  // read stands failed (`standsFailed`).
  if (standsFailed(handoff)) {
    return (
      <span className="mt-1 block" id={id} ref={failure}>
        <DetailsErrorLine
          failure={handoff.errorUpdateCount}
          message={ANSWER_LINES.handoff_unreadable}
          // The Retry goes with its line once the read it sends lands. The press moves no focus,
          // and the line's going hands it on (`retry`). A press while that read is out joins it
          // (`retryRead`), so a second press made before React draws the first one's read sends
          // nothing more.
          onRetry={retryRead(handoff)}
          {...retry}
          retrying={handoff.isFetching}
        />
      </span>
    );
  }
  if (handoff.data === undefined) {
    return (
      <span
        className={`mt-1 block text-xs outline-none ${pageQuietText}`}
        id={id}
        ref={loadingLeft}
        role="status"
        tabIndex={-1}
        {...pageChrome}
      >
        {ANSWER_LINES.loading_handoff}
      </span>
    );
  }
  // The fields hold the body whatever a later read's outcome, as the history holds its log: an
  // invalidation of every read after a write reads the body again, and a read that fails then
  // draws no line over the fields it held.
  if (fields === undefined) {
    return (
      <pre
        className={`${handoffPre} outline-none`}
        data-handoff-raw
        id={id}
        tabIndex={-1}
        {...authorText}
      >
        {handoff.data.body}
      </pre>
    );
  }
  return (
    <dl
      className={`mt-1 flex flex-col gap-2 text-xs outline-none ${pageInkText}`}
      id={id}
      tabIndex={-1}
      {...authorText}
    >
      {fields.map(({ entry, field, label, shape, text }) => {
        const known = Object.hasOwn(KNOWN_FIELDS, field) ? KNOWN_FIELDS[field] : undefined;
        // The element keeps the field's key, which a reader of the page selects by; the label
        // is the page's name for the field, or the words its key spells.
        return (
          <div data-handoff-field={field} key={`${field}/${label}`}>
            <dt className={`font-medium ${pageMutedText}`}>
              {entry !== undefined ? (
                <span {...pageChrome}>
                  {CONTROL_LABELS.handoff_fields.proof} <span {...authorText}>{entry}</span>
                </span>
              ) : known !== undefined ? (
                <span {...pageChrome}>{known}</span>
              ) : (
                <span {...authorText}>{keyWords(field)}</span>
              )}
            </dt>
            <dd className="min-w-0">
              {shape === "json" ? (
                <pre className={handoffPre}>{text}</pre>
              ) : (
                <span className="break-words whitespace-pre-wrap">{text}</span>
              )}
            </dd>
          </div>
        );
      })}
    </dl>
  );
}

/** `body`'s rows as `handoffFields` reads them, each entry of a `proof` list numbered, or
 *  undefined for a body that is no JSON object. */
function fieldRows(body: string): FieldRow[] | undefined {
  const fields = handoffFields(body);
  if (fields === undefined) return undefined;
  const rows: FieldRow[] = [];
  let entries = 0;
  for (const field of fields) {
    // `handoffFields` names a `proof` list's entries apart from a `proof` that is no list.
    const entry = field.field === "proof" && field.label !== field.field;
    if (entry) entries += 1;
    rows.push({ ...field, entry: entry ? entries : undefined });
  }
  return rows;
}

/** A handoff field's key as the words its parts spell, in sentence case: `filesChanged`,
 *  `files_changed` and `files-changed` all read "Files changed", and a word in capitals, such as
 *  `CI`, keeps them. A key that spells no word reads as itself. */
function keyWords(key: string): string {
  const words = key
    .replace(/(\p{Ll}|\p{N})(\p{Lu})/gu, "$1 $2")
    .replace(/(\p{Lu})(\p{Lu}\p{Ll})/gu, "$1 $2")
    .split(/[\s_-]+/u)
    .filter((word) => word !== "")
    .map((word) => (word.length > 1 && word === word.toUpperCase() ? word : word.toLowerCase()));
  return words.length === 0 ? key : sentenceCase(words.join(" "));
}
