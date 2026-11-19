import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useId, useMemo, useState } from "react";

import { api } from "../../api/client";
import {
  dangerText,
  pageInkText,
  pageMutedText,
  pageQuietText,
  pageRowAct,
  surfaceMutedBg,
  textMutedOnSurfaceMuted,
} from "../../theme/classes";
import { handoffFields } from "../conversation/conversation-model";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";

/** How a handoff shows JSON: an array or object field, or a body that is not a JSON object. */
const handoffPre = `mt-1 max-h-64 overflow-auto rounded-md p-2 text-xs whitespace-pre-wrap ${surfaceMutedBg} ${textMutedOnSurfaceMuted}`;

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
    () => (handoff.data === undefined ? undefined : handoffFields(handoff.data.body)),
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
      <span className={`mt-1 block text-xs ${dangerText}`} id={id} {...pageChrome}>
        {ANSWER_LINES.handoff_unreadable}
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
      {fields.map(({ field, json, label, text }) => (
        <div data-handoff-field={field} key={`${field}/${label}`}>
          <dt className={`font-medium ${pageMutedText}`}>{label}</dt>
          <dd className="min-w-0">
            {json ? (
              <pre className={handoffPre}>{text}</pre>
            ) : (
              <span className="break-words whitespace-pre-wrap">{text}</span>
            )}
          </dd>
        </div>
      ))}
    </dl>
  );
}
