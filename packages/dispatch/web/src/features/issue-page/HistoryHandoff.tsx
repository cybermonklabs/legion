import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useId, useMemo, useState } from "react";

import { api } from "../../api/client";
import {
  pageInkText,
  pageMarkBg,
  pageMutedText,
  pageQuietText,
  pageRowAct,
} from "../../theme/classes";
import { type HandoffField, handoffFields } from "../conversation/conversation-model";
import { DetailsErrorLine } from "./details-error";
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
 * The disclosure on a handoff's line in the issue's history (dispatch://INFRA-1196 R15, R19:
 * "the conversation's handoff cards ... the history in the panel"): Show fields opens the
 * version's body under the line - each top-level field a labelled row, the `proof` list one row
 * per entry, as the old conversation's handoff card drew it - and Hide fields closes it. The
 * body is read when the reader opens it, and a version never changes once written, so the read
 * is kept for the page's life.
 */
export function HistoryHandoff({ handoffId }: { handoffId: string }): ReactNode {
  const [expanded, setExpanded] = useState(false);
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

function HandoffFields({ handoffId, id }: { handoffId: string; id: string }): ReactNode {
  const handoff = useQuery({
    queryKey: ["handoff", handoffId],
    queryFn: () => api.getHandoff(handoffId),
    staleTime: Number.POSITIVE_INFINITY,
  });
  const fields = useMemo(
    () => (handoff.data === undefined ? undefined : fieldRows(handoff.data.body)),
    [handoff.data]
  );
  if (handoff.isPending) {
    return (
      <span className={`mt-1 block text-xs ${pageQuietText}`} id={id} {...pageChrome}>
        {ANSWER_LINES.loading_handoff}
      </span>
    );
  }
  if (handoff.isError) {
    return (
      <span className="mt-1 block" id={id}>
        <DetailsErrorLine
          message={ANSWER_LINES.handoff_unreadable}
          onRetry={() => void handoff.refetch()}
          retrying={handoff.isFetching}
        />
      </span>
    );
  }
  if (fields === undefined) {
    return (
      <pre className={handoffPre} data-handoff-raw id={id} {...authorText}>
        {handoff.data.body}
      </pre>
    );
  }
  return (
    <dl className={`mt-1 flex flex-col gap-2 text-xs ${pageInkText}`} id={id} {...authorText}>
      {fields.map(({ entry, field, json, label, text }) => {
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
              {json ? (
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
