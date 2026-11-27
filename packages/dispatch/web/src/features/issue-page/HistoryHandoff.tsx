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
import { MarkdownBody } from "../refs/MarkdownBody";
import { standsFailed } from "../refs/reference-target";
import { DetailsErrorLine, retryRead, useRetryHandOff } from "./details-error";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS, sentenceCase } from "./page-words";

/** The box a list or an object at the top of a handoff's body is drawn in: at most 16 rem high,
 *  scrolling inside itself, so an entry's open height stays bounded, in the muted grey on the mark
 *  grey, as a node's body draws a check's command. */
const handoffBox = `mt-1 max-h-64 overflow-auto rounded-md p-2 text-xs ${pageMarkBg} ${pageMutedText}`;

/** The page's names of the fields every Legion handoff record carries. */
const KNOWN_FIELDS: Readonly<Record<string, string>> = CONTROL_LABELS.handoff_fields;

/** One row of a handoff's opened fields: `handoffFields`' row, and which entry of the `proof`
 *  list it is, counted from 1, where it is one. */
interface FieldRow extends HandoffField {
  readonly entry: number | undefined;
}

/** The handoffs the reader folded on one issue's page (the plan's D12): whether a handoff is
 *  folded, and the fold or the open a press of its control makes. */
export interface HandoffFold {
  readonly folded: (handoffId: string) => boolean;
  readonly setFolded: (handoffId: string, folded: boolean) => void;
}

/** No handoff folded. */
const NONE_FOLDED: ReadonlySet<string> = new Set();

/**
 * The handoffs the reader folded on the page of `issueKey` (`HandoffFold`), held by the one that
 * calls it: `DetailsHistory`, which stays mounted for the life of the panel, so a fold lasts
 * while Details shuts and opens, while a live update or the next page draws the history again,
 * and while a run takes the handoff into its entry. It forgets every fold when the issue changes.
 */
export function useHandoffFold(issueKey: string): HandoffFold {
  const [held, setHeld] = useState<{ readonly ids: ReadonlySet<string>; readonly issue: string }>(
    () => ({ ids: NONE_FOLDED, issue: issueKey })
  );
  if (held.issue !== issueKey) setHeld({ ids: NONE_FOLDED, issue: issueKey });
  const ids = held.issue === issueKey ? held.ids : NONE_FOLDED;
  return useMemo(
    () => ({
      folded: (handoffId) => ids.has(handoffId),
      setFolded: (handoffId, folded) =>
        setHeld((current) => {
          const next = new Set(current.issue === issueKey ? current.ids : NONE_FOLDED);
          if (folded) next.add(handoffId);
          else next.delete(handoffId);
          return { ids: next, issue: issueKey };
        }),
    }),
    [ids, issueKey]
  );
}

/**
 * The fields of a handoff of the issue's history, which took over the conversation's handoff
 * cards: drawn open - each top-level field of the version's body a labelled row, the `proof`
 * list one row per entry, as the old conversation's handoff card drew it, a list as one numbered
 * entry for each item and an object as each key with its value - with no press. Only the reader
 * folds them, by Hide fields, and Show fields opens them again; whether they are folded is
 * `folded`, which the page holds for the issue (`useHandoffFold`), and a press says its fold
 * through `onFold`. The body is read as the fields are drawn, and a version never changes once
 * written, so the read is kept for the page's life: it is never stale, and folding the fields
 * never lets it go.
 */
export function HistoryHandoff({
  folded,
  handoffId,
  onFold,
}: {
  folded: boolean;
  handoffId: string;
  onFold: (folded: boolean) => void;
}): ReactNode {
  const fieldsId = useId();
  const expanded = !folded;
  return (
    <span className="basis-full">
      <button
        aria-controls={expanded ? fieldsId : undefined}
        aria-expanded={expanded}
        className={`dispatch-inline-link text-xs font-medium ${pageMutedText} ${pageRowAct}`}
        onClick={() => onFold(expanded)}
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
  const trees = handoff.data.field_trees;
  if (fields === undefined) {
    return (
      <div
        className={`mt-1 min-w-0 text-xs outline-none [overflow-wrap:anywhere] ${pageInkText}`}
        data-handoff-raw
        data-node-words=""
        id={id}
        tabIndex={-1}
        {...authorText}
      >
        <MarkdownBody
          markdown={handoff.data.body}
          surface="issue-page-history-handoff-body"
          tree={trees !== undefined && Object.hasOwn(trees, "") ? trees[""] : undefined}
        />
      </div>
    );
  }
  return (
    <dl
      className={`mt-1 flex flex-col gap-2 text-xs outline-none ${pageInkText}`}
      id={id}
      tabIndex={-1}
      {...authorText}
    >
      {fields.map(({ entry, field, label, path, shape, text }) => {
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
              {shape === "prose" ? (
                <div className="min-w-0 [overflow-wrap:anywhere]" data-node-words="">
                  <MarkdownBody
                    markdown={text}
                    surface="issue-page-history-handoff-body"
                    tree={
                      trees !== undefined && Object.hasOwn(trees, path) ? trees[path] : undefined
                    }
                  />
                </div>
              ) : shape === "json" ? (
                // The session's data: a list one numbered entry for each item, an object each key
                // with its value, the reading list's `issue-page-history-handoff-data`.
                <TopLevelData text={text} />
              ) : (
                <span className="break-words whitespace-pre-wrap">
                  {/* A number, a boolean or null, as its JSON text: the reading list's
                      `issue-page-history-handoff-data`. */}
                  {text}
                </span>
              )}
            </dd>
          </div>
        );
      })}
    </dl>
  );
}

/** A list or an object at the top of a handoff's body, which `handoffFields` gives as its JSON
 *  text: drawn by `HandoffData` in the box that bounds an entry's open height (`handoffBox`), and
 *  an empty one as None, which needs no box. */
function TopLevelData({ text }: { text: string }): ReactNode {
  // `handoffFields` gives a list or an object alone as JSON text.
  const value = JSON.parse(text) as object;
  if (Object.keys(value).length === 0) return <NoValue />;
  return (
    <div className={handoffBox}>
      <HandoffData value={value} />
    </div>
  );
}

/** A value that holds nothing, read as every row of Details reads a property with no value. */
function NoValue(): ReactNode {
  return (
    <span className={pageQuietText} {...pageChrome}>
      {CONTROL_LABELS.none}
    </span>
  );
}

/**
 * A value inside a handoff's body, as the session wrote it (the plan's D11): a list as one
 * numbered entry for each item, an object as each key, in the words it spells (`keyWords`), with
 * its value, each value inside them by the same rule; a string as plain text; a number, a boolean
 * or null as its JSON text; and an empty list or object as None. Every word of it is the
 * session's but None.
 */
function HandoffData({ value }: { value: unknown }): ReactNode {
  if (value !== null && typeof value === "object" && Object.keys(value).length === 0) {
    return <NoValue />;
  }
  if (Array.isArray(value)) {
    return (
      <ol className="flex min-w-0 list-decimal flex-col gap-1 pl-5" data-handoff-list="">
        {value.map((item, index) => (
          // biome-ignore lint/suspicious/noArrayIndexKey: a list can hold one value twice, so an item's place is its identity
          <li className="min-w-0" key={index}>
            <HandoffData value={item} />
          </li>
        ))}
      </ol>
    );
  }
  if (value !== null && typeof value === "object") {
    return (
      <dl className="flex min-w-0 flex-col gap-1">
        {Object.entries(value).map(([key, member]) => (
          <div className="min-w-0" data-handoff-key={key} key={key}>
            <dt className={`font-medium ${pageMutedText}`}>
              <span {...authorText}>{keyWords(key)}</span>
            </dt>
            <dd className="min-w-0 pl-3">
              <HandoffData value={member} />
            </dd>
          </div>
        ))}
      </dl>
    );
  }
  return (
    <span className={`break-words whitespace-pre-wrap ${pageInkText}`}>
      {typeof value === "string" ? value : JSON.stringify(value)}
    </span>
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
