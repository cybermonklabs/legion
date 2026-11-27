import { skipToken, useQuery, useQueryClient } from "@tanstack/react-query";
import {
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
import { type HandoffField, handoffFields } from "../conversation/conversation-model";
import { MarkdownBody } from "../refs/MarkdownBody";
import { DetailsErrorLine, useRetryHandOff } from "./details-error";
import { HandoffBodyLoader, type HeldHandoffBody, handoffBodyKey } from "./handoff-body-loader";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS, sentenceCase } from "./page-words";

/** How far past the view of the box Details scrolls in, above it and below it, a handoff's entry
 *  is near enough for its body to be read: one screen each way, as `clamp-measurer.ts` measures
 *  the outline's rows. */
const NEAR_BAND = "100% 0px";

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
 * The handoff entries of one history within one screen of the view of the box Details scrolls in
 * (`NEAR_BAND` above and below it), seen by one `IntersectionObserver` whose root is that box
 * (`observeIn`), and told, in the order drawn, to the bodies' loader of the period Details is
 * open (`use`), with each scroll the reader makes of that box (the plan's D13). A handoff's place
 * is watched only while its fields are open (`watch`), so a folded handoff is no part of the band
 * and its body is read only once it is opened. The band is told once the observer has first
 * reported, so the history's first request names every entry its first view holds, and once for
 * each task's changes, as the browser reports every entry an observation found in one call. It
 * keeps the history's landing too (`landed`), so the loader of a later period of Details starts
 * at once.
 */
class HandoffBand {
  /** The handoff whose place each element watched is. */
  readonly #watched = new Map<Element, string>();
  /** The places watched that stand in the band. */
  readonly #near = new Set<Element>();
  #observer: IntersectionObserver | undefined;
  #root: Element | null = null;
  #scrolls: AbortController | undefined;
  /** Whether the observer has reported since it began to watch. */
  #reported = false;
  /** Whether the band is to be told at the end of the task. */
  #telling = false;
  #loader: HandoffBodyLoader | undefined;
  /** The history's landing, with the handoff it names, once it has landed. */
  #landing: { readonly first: string | undefined } | undefined;

  /** Watches `element`, the place of handoff `id`'s open fields, until the answer is called. */
  readonly watch = (id: string, element: Element): (() => void) => {
    this.#watched.set(element, id);
    this.#observer?.observe(element);
    return () => {
      this.#watched.delete(element);
      this.#observer?.unobserve(element);
      if (this.#near.delete(element)) this.#tell();
    };
  };

  /** Watches the band in the view of `root`, the box Details scrolls in, or of the document's
   *  window where the history scrolls in no box of its own, until `stop`. */
  observeIn(root: Element | null): void {
    if (this.#observer !== undefined && root === this.#root) return;
    this.stop();
    this.#root = root;
    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (!this.#watched.has(entry.target)) continue;
          if (entry.isIntersecting) this.#near.add(entry.target);
          else this.#near.delete(entry.target);
        }
        this.#reported = true;
        this.#tell();
      },
      { root, rootMargin: NEAR_BAND }
    );
    this.#observer = observer;
    for (const element of this.#watched.keys()) observer.observe(element);
    // The reader's own scroll of the box: a turn of the wheel, a touch's move or a key, the acts
    // the history's anchor reads as the reader's too (`ViewportAnchor`). A scroll event alone is
    // no reader's: the history's landing, the anchor's put-backs and the browser's own scroll
    // anchoring raise it as well, and as an answer's bodies are drawn, which would send the ids
    // that answer left at once rather than 2 seconds after it is drawn.
    this.#scrolls = new AbortController();
    for (const type of ["keydown", "touchmove", "wheel"]) {
      (root ?? window).addEventListener(type, () => this.#loader?.scrolled(), {
        passive: true,
        signal: this.#scrolls.signal,
      });
    }
  }

  stop(): void {
    this.#observer?.disconnect();
    this.#observer = undefined;
    this.#scrolls?.abort();
    this.#scrolls = undefined;
    this.#near.clear();
    this.#tell();
    this.#reported = false;
  }

  /** Tells `loader`, the loader of the period Details is open, the band and the landing from
   *  now on; `undefined` while Details is shut. */
  use(loader: HandoffBodyLoader | undefined): void {
    this.#loader = loader;
    this.#tell();
    if (this.#landing !== undefined) loader?.landed(this.#landing.first);
  }

  /** The history has landed, on the handoff `first` where its address names one. */
  landed(first?: string): void {
    if (this.#landing !== undefined) return;
    this.#landing = { first };
    this.#loader?.landed(first);
  }

  /** Tells the loader the band at the end of this task, in the order drawn. */
  #tell(): void {
    if (!this.#reported || this.#telling) return;
    this.#telling = true;
    queueMicrotask(() => {
      this.#telling = false;
      const near = [...this.#near].sort((one, other) =>
        one.compareDocumentPosition(other) & Node.DOCUMENT_POSITION_FOLLOWING ? -1 : 1
      );
      this.#loader?.band(near.flatMap((element) => this.#watched.get(element) ?? []));
    });
  }
}

/** What a handoff of the history reads of its body's read: the loader of the period Details is
 *  open, the last one's while it is shut, and the band its place is watched in. */
export interface HandoffBodies {
  readonly loader: HandoffBodyLoader | undefined;
  readonly watch: HandoffBand["watch"];
}

/**
 * The bodies' reads of the history of `issueKey` (the plan's D13): a band of the issue's own,
 * which the history gives the box Details scrolls in and its landing (`HandoffBand`), and a
 * loader made for each period Details is open (`open`) and closed as it shuts, so it reads
 * nothing while Details is shut, though the history stays drawn there at `/activity`. Each
 * period's loader is made in an effect, so the second run of a strict effect makes another one
 * rather than leaving one closed. The history hands each of its handoffs `value`.
 */
export function useHandoffBodies(
  issueKey: string,
  open: boolean
): { readonly band: HandoffBand; readonly value: HandoffBodies } {
  const client = useQueryClient();
  const [held, setHeld] = useState(() => ({ band: new HandoffBand(), issue: issueKey }));
  let band = held.band;
  if (held.issue !== issueKey) {
    band = new HandoffBand();
    setHeld({ band, issue: issueKey });
  }
  const [loader, setLoader] = useState<HandoffBodyLoader | undefined>(undefined);
  useEffect(() => {
    if (!open) return;
    const made = new HandoffBodyLoader(issueKey, client);
    setLoader(made);
    band.use(made);
    return () => {
      made.close();
      band.use(undefined);
    };
  }, [band, client, issueKey, open]);
  const value = useMemo(() => ({ loader, watch: band.watch }), [band, loader]);
  return { band, value };
}

/**
 * The fields of a handoff of the issue's history, which took over the conversation's handoff
 * cards: drawn open - each top-level field of the version's body a labelled row, the `proof`
 * list one row per entry, as the old conversation's handoff card drew it, a list as one numbered
 * entry for each item and an object as each key with its value - with no press. Only the reader
 * folds them, by Hide fields, and Show fields opens them again; whether they are folded is
 * `folded`, which the page holds for the issue (`useHandoffFold`), and a press says its fold
 * through `onFold`. The body is read by the history's bodies' loader once the open fields come
 * within one screen of Details' view (`bodies`), and a version never changes once written, so it
 * is kept for the page's life: it is never read again, and folding the fields never lets it go.
 */
export function HistoryHandoff({
  bodies,
  folded,
  handoffId,
  onFold,
}: {
  bodies: HandoffBodies;
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
  // The body the loader keeps for the page's life: this query never reads it itself.
  const held = useQuery<HeldHandoffBody>({
    gcTime: Number.POSITIVE_INFINITY,
    queryFn: skipToken,
    queryKey: handoffBodyKey(handoffId),
    staleTime: Number.POSITIVE_INFINITY,
  });
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
  const fields = useMemo(() => (body === undefined ? undefined : fieldRows(body.body)), [body]);
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
      {fields.map(({ entry, field, label, path, shape, text }) => {
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
                <span className="whitespace-pre-wrap [overflow-wrap:anywhere]">
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
            <dt className={`font-medium [overflow-wrap:anywhere] ${pageMutedText}`}>
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
  // A path, a commit's id or an address with no space in it wraps at any character, so its box
  // is no wider than the entry it stands in.
  return (
    <span className={`whitespace-pre-wrap [overflow-wrap:anywhere] ${pageInkText}`}>
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
