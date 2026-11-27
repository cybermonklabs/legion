import { useQuery } from "@tanstack/react-query";
import {
  memo,
  type ReactNode,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";

import { useFocusHandOff } from "../../hooks/useFocusHandOff";
import {
  pageInkText,
  pageMarkBg,
  pageMutedText,
  pageQuietText,
  pageRowAct,
} from "../../theme/classes";
import { MarkdownBody } from "../refs/MarkdownBody";
import { DetailsErrorLine, useRetryHandOff } from "./details-error";
import type { HandoffBodyReads } from "./handoff-band";
import { type HandoffBodyLoader, handoffBodyQuery } from "./handoff-body-loader";
import { handoffRows, jsonText } from "./handoff-rows";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS, sentenceCase } from "./page-words";

/** The box a list or an object at the top of a handoff's body is drawn in: at most 16 rem high,
 *  scrolling inside itself, so an entry's open height stays bounded, in the muted grey on the mark
 *  grey, as a node's body draws a check's command. */
const handoffBox = `mt-1 max-h-64 overflow-auto rounded-md p-2 text-xs ${pageMarkBg} ${pageMutedText}`;

/**
 * How many levels of lists and objects inside a handoff's field `HandoffData` draws as lists and
 * keys: a list or an object below them is drawn as its JSON text, in a box of its own
 * (`handoffJsonBox`), with no `HandoffData` inside it. Any agent session can record a handoff on
 * any open issue, nested as deep as the server's JSON check takes, 10,000 levels, and a handoff
 * is never changed or deleted, so no walk of a value recurses past this bound: one session's
 * handoff would make Details throw for every reader of the issue. Each level drawn is a few
 * elements, which React's commit walks by recursion, so a body of a few KB drawn to its bottom,
 * nested 3,000 deep, overflows the call stack. `JSON.stringify` recurses once a level as well,
 * and in V8, Chrome's engine, throws from about 4,500 levels, a body of 9 KB. So the body is
 * parsed once (`handoffRows`), each list and object of its rows is the value it was parsed to,
 * and the text past the bound is written by a loop with no recursion (`jsonText`), with no
 * indent: an indent of two spaces a level would write a value nested 3,000 deep as millions of
 * characters. The bodies captured from real issues (`fixtures/handoff-bodies.json`) nest at most
 * 3 levels inside a field, so R2's lists and keys hold for every one of them.
 */
const HANDOFF_DATA_LEVELS = 8;

/** The box a list or an object past `HANDOFF_DATA_LEVELS` is drawn in as its JSON text: the box
 *  of a list at the top (`handoffBox`), wrapping at any character so it is no wider than the
 *  entry it stands in. */
const handoffJsonBox = `${handoffBox} whitespace-pre-wrap [overflow-wrap:anywhere]`;

/** The page's names of the fields every Legion handoff record carries. */
const KNOWN_FIELDS: Readonly<Record<string, string>> = CONTROL_LABELS.handoff_fields;

/** The handoffs the reader folded on one issue's page (the plan's D12): whether a handoff is
 *  folded, and the fold or the open a press of its control makes. */
export interface HandoffFold {
  readonly folded: (handoffId: string) => boolean;
  readonly setFolded: (handoffId: string, folded: boolean) => void;
}

/** No handoff folded, and no address followed. */
const NONE_FOLDED: ReadonlySet<string> = new Set();

/** The handoffs folded on one issue's page, and the followings of a handoff's address that opened
 *  theirs (`useHandoffFold`). */
interface HeldFold {
  readonly ids: ReadonlySet<string>;
  readonly issue: string;
  readonly opened: ReadonlySet<string>;
}

/**
 * The handoffs the reader folded on the page of `issueKey` (`HandoffFold`), held by the one that
 * calls it: `DetailsHistory`, which stays mounted for the life of the panel, so a fold lasts
 * while Details shuts and opens, while a live update or the next page draws the history again,
 * and while a run takes the handoff into its entry. It forgets every fold when the issue changes.
 * A handoff's page address (`addressed`, with the following it is at, `useFollowing`) opens that
 * handoff once for each following, as it is drawn: the reader's next fold there stays, and Back or
 * Forward to a following it opened at already leaves the fold the reader made since.
 */
export function useHandoffFold(
  issueKey: string,
  addressed?: { readonly following: string; readonly id: string }
): HandoffFold {
  const [held, setHeld] = useState<HeldFold>(() => ({
    ids: NONE_FOLDED,
    issue: issueKey,
    opened: NONE_FOLDED,
  }));
  let current =
    held.issue === issueKey ? held : { ids: NONE_FOLDED, issue: issueKey, opened: NONE_FOLDED };
  if (addressed !== undefined && !current.opened.has(addressed.following)) {
    const ids = new Set(current.ids);
    ids.delete(addressed.id);
    current = { ids, issue: issueKey, opened: new Set([...current.opened, addressed.following]) };
  }
  if (current !== held) setHeld(current);
  const ids = current.ids;
  return useMemo(
    () => ({
      folded: (handoffId) => ids.has(handoffId),
      setFolded: (handoffId, folded) =>
        setHeld((now) => {
          const kept = now.issue === issueKey ? now : undefined;
          const next = new Set(kept?.ids ?? NONE_FOLDED);
          if (folded) next.add(handoffId);
          else next.delete(handoffId);
          return { ids: next, issue: issueKey, opened: kept?.opened ?? NONE_FOLDED };
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
 * through `onFold`. The body is read by the history's bodies' loader once the open fields come
 * within one screen of Details' view (`bodies`). A version never changes once written, so the
 * body is kept while its open fields draw it and for the query cache's 5 minutes after
 * (`handoffBodyQuery`): fields opened again within them read nothing, and later ones read it again.
 */
export function HistoryHandoff({
  bodies,
  folded,
  handoffId,
  onFold,
}: {
  bodies: HandoffBodyReads;
  folded: boolean;
  handoffId: string;
  onFold: (folded: boolean) => void;
}): ReactNode {
  const fieldsId = useId();
  const expanded = !folded;
  const { loader, watch } = bodies;
  // The place of the fields, which the band watches while they are open.
  const place = useRef<HTMLSpanElement>(null);
  useLayoutEffect(() => {
    const element = place.current;
    if (element === null || !expanded) return;
    return watch(handoffId, element);
  }, [expanded, handoffId, watch]);
  // A row of its own in the line it is drawn in, as narrow as that line, so a long word in a
  // value wraps inside it rather than widening Details.
  return (
    <span className="min-w-0 basis-full" ref={place}>
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
      {expanded ? <HandoffFields handoffId={handoffId} id={fieldsId} loader={loader} /> : null}
    </span>
  );
}

/** A handoff's opened fields, or the line that says they are being read, or that the read
 *  failed; `id` names whichever of them stands in the fields' place. */
function HandoffFields({
  handoffId,
  id,
  loader,
}: {
  handoffId: string;
  id: string;
  loader: HandoffBodyLoader | undefined;
}): ReactNode {
  const read = useSyncExternalStore(
    useCallback((listener: () => void) => loader?.subscribe(listener) ?? (() => {}), [loader]),
    () => loader?.state(handoffId)
  );
  const failures = read?.failures ?? 0;
  // The body the loader set, kept while this query observes it and for the cache's 5 minutes
  // after (`handoffBodyQuery`): this query never reads it itself.
  const held = useQuery({ ...handoffBodyQuery(handoffId), staleTime: Number.POSITIVE_INFINITY });
  const body = held.data;
  // What an answer brought is drawn: the loader's wait for the ids it left starts there.
  useEffect(() => {
    if (body !== undefined || failures > 0) loader?.drawn();
  }, [body, failures, loader]);
  const failure = useRef<HTMLSpanElement>(null);
  // A focus on a line that goes - the Retry of the failure line once the read the reader's press
  // of it sent lands, or the loading line - goes on to what the reader's press asked for, where no
  // Enter undoes it: the Retry of a failure line drawn in its place, else what stands in the
  // fields' place, the fields once the read has landed. The failure line stays while the read its
  // Retry sent is out, so the loading line never takes the focus from the Retry. The toggle above
  // them would not do: a second press of the Retry's key - a key that repeated, or a reader who
  // saw nothing happen and pressed again - would land on it and shut the fields. The Retry hands
  // it on only after the reader's press of it (`useRetryHandOff`): a line taken away by a read the
  // reader did not press for - another entry's Retry that asked for this body again with its own -
  // leaves the focus on the page's body, as on every page of Dispatch. The loading line, which no
  // Tab reaches, holds the focus only where the reader put it there, and goes only as the read it
  // stands for answers, so it hands the focus on with no further press.
  const handOffTo = () =>
    failure.current?.querySelector<HTMLElement>("button") ?? document.getElementById(id);
  const retry = useRetryHandOff(handOffTo);
  const loadingLeft = useFocusHandOff(handOffTo);
  const fields = useMemo(() => (body === undefined ? undefined : handoffRows(body.body)), [body]);
  // The failure line stands in the fields' place, and in place of the loading line, while the
  // body is not held and its last read failed.
  if (body === undefined && failures > 0) {
    return (
      <span className="mt-1 block" id={id} ref={failure}>
        <DetailsErrorLine
          failure={failures}
          message={ANSWER_LINES.handoff_unreadable}
          // The Retry goes with its line once the read it sends lands. The press moves no focus,
          // and the line's going hands it on (`retry`). A press while that read is out sends
          // nothing more (`HandoffBodyLoader.retry`), so a second press made before React draws
          // the first one's read sends nothing either.
          onRetry={() => loader?.retry(handoffId)}
          {...retry}
          retrying={read?.reading === true}
        />
      </span>
    );
  }
  if (body === undefined) {
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
  // Each text is drawn from the reading the server sent beside the body (`field_trees`); an
  // answer whose first entry alone passes the bodies' limit carries no trees, and the renderer
  // reads that one body itself.
  const trees = body.field_trees;
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
          markdown={body.body}
          surface="issue-page-history-handoff-body"
          tree={trees !== undefined && Object.hasOwn(trees, "") ? trees[""] : undefined}
        />
      </div>
    );
  }
  return (
    <dl
      className={`mt-1 flex min-w-0 flex-col gap-2 text-xs outline-none ${pageInkText}`}
      id={id}
      tabIndex={-1}
      {...authorText}
    >
      {fields.map((row) => {
        const { entry, field, label, path } = row;
        const known = Object.hasOwn(KNOWN_FIELDS, field) ? KNOWN_FIELDS[field] : undefined;
        // The element keeps the field's key, which a reader of the page selects by; the label
        // is the page's name for the field, or the words its key spells.
        return (
          <div className="min-w-0" data-handoff-field={field} key={`${field}/${label}`}>
            <dt className={`font-medium [overflow-wrap:anywhere] ${pageMutedText}`}>
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
              {row.shape === "prose" ? (
                <div className="min-w-0 [overflow-wrap:anywhere]" data-node-words="">
                  <MarkdownBody
                    markdown={row.text}
                    surface="issue-page-history-handoff-body"
                    tree={
                      trees !== undefined && Object.hasOwn(trees, path) ? trees[path] : undefined
                    }
                  />
                </div>
              ) : row.shape === "json" ? (
                // The session's data: a list one numbered entry for each item, an object each key
                // with its value, the reading list's `issue-page-history-handoff-data`.
                <TopLevelData value={row.value} />
              ) : (
                <span className="whitespace-pre-wrap [overflow-wrap:anywhere]">
                  {/* A number, a boolean or null, as its JSON text: the reading list's
                      `issue-page-history-handoff-data`. */}
                  {row.text}
                </span>
              )}
            </dd>
          </div>
        );
      })}
    </dl>
  );
}

/** A list or an object at the top of a handoff's body, the value its row holds (`handoffRows`):
 *  drawn by `HandoffData` in the box that bounds an entry's open height (`handoffBox`), and an
 *  empty one as None, which needs no box. */
const TopLevelData = memo(function TopLevelData({ value }: { value: object }): ReactNode {
  if (Object.keys(value).length === 0) return <NoValue />;
  return (
    <div className={handoffBox}>
      <HandoffData level={1} value={value} />
    </div>
  );
});

/** A value that holds nothing, read as every row of Details reads a property with no value. */
export function NoValue(): ReactNode {
  return (
    <span className={pageQuietText} {...pageChrome}>
      {CONTROL_LABELS.none}
    </span>
  );
}

/**
 * A value inside a handoff's body, as the session wrote it (the plan's D11), at `level` of its
 * field's lists and objects, 1 for the field's own value: a list as one numbered entry for each
 * item, an object as each key, in the words it spells (`keyWords`), with its value, each value
 * inside them by the same rule; a string as plain text; a number, a boolean or null as its JSON
 * text; and an empty list or object as None. A list or an object past `HANDOFF_DATA_LEVELS` is
 * its JSON text (`jsonText`) in a box of its own, so the elements drawn stop there however deep
 * it nests, and so does every walk of it but the loop that writes that text.
 * Every word of it is the session's but None.
 */
function HandoffData({ level, value }: { level: number; value: unknown }): ReactNode {
  if (value !== null && typeof value === "object" && Object.keys(value).length === 0) {
    return <NoValue />;
  }
  if (value !== null && typeof value === "object" && level > HANDOFF_DATA_LEVELS) {
    return <pre className={handoffJsonBox}>{jsonText(value)}</pre>;
  }
  if (Array.isArray(value)) {
    return (
      <ol className="flex min-w-0 list-decimal flex-col gap-1 pl-5" data-handoff-list="">
        {value.map((item, index) => (
          // biome-ignore lint/suspicious/noArrayIndexKey: a list can hold one value twice, so an item's place is its identity
          <li className="min-w-0" key={index}>
            <HandoffData level={level + 1} value={item} />
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
            <dt className={`font-medium [overflow-wrap:anywhere] ${pageMutedText}`}>
              <span {...authorText}>{keyWords(key)}</span>
            </dt>
            <dd className="min-w-0 pl-3">
              <HandoffData level={level + 1} value={member} />
            </dd>
          </div>
        ))}
      </dl>
    );
  }
  // A path, a commit's id or an address with no space in it wraps at any character, so its box
  // is no wider than the entry it stands in.
  return (
    <span className={`whitespace-pre-wrap [overflow-wrap:anywhere] ${pageInkText}`}>
      {typeof value === "string" ? value : JSON.stringify(value)}
    </span>
  );
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
