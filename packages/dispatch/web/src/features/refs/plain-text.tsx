import type { DispatchReadingSurface } from "@legion/contracts";
import { mentionDrawn } from "@legion/contracts/dispatch-mentions";
import { type ReactNode, useCallback, useEffect, useLayoutEffect, useMemo, useState } from "react";

import type { DisplayTree } from "../../api/types";
import {
  citedReferences,
  type MarkdownHeadline,
  type ReferenceName,
  type ReferenceNameLookup,
  type ReferenceNaming,
  useMarkdownHeadline,
} from "./markdown-engine";
import { isPartyRoute } from "./party-references";
import { useProjectKeys } from "./project-keys";
import { ReferenceHostIs } from "./reference-host";
import { type KnownReferences, useKnownReferences } from "./reference-names";
import {
  questionReferences,
  questionsQuery,
  useReferenceLabel,
  useReferenceQuery,
  useReferenceTarget,
} from "./reference-target";
import { RecordIssue } from "./role-holders";
import { type DispatchReferenceRoute, parseNodeReference, shortForm } from "./routes";
import type { TextSource } from "./text-references";

/** No reference read yet, shared so a line that names none keeps one identity across renders. */
const NO_NAMES: ReadonlyMap<string, ReferenceName> = new Map();

/** The references of a line the projection has not answered for yet: none. */
const NO_ADDRESSES: readonly string[] = [];

/** What a reference reads as in plain words once its target has answered (`ReferenceName`): the
 *  name the painted link reads for it (`RefLink`), which for an issue is its title, which the
 *  projection writes after the issue's key as a project's issue list writes a key beside one
 *  (`markdown-engine.ts`'s `standingName`), and for a party's mention what the painted mention
 *  draws (`@legion/contracts`'s `mentionDrawn`): the mention sign and that name, or the sign and
 *  the party's short form where it exists with no name. A target Dispatch answered does not
 *  exist, or exists with no name of its own, has no name, and reads as the words its author
 *  wrote (R8). Undefined while the target has not answered, and for one the reader may not see,
 *  where the projection keeps the words written, as the painted link does until its title
 *  resolves. */
function referenceAnswer(
  route: DispatchReferenceRoute,
  { known, missing, title }: { known: boolean; missing: boolean; title: string | undefined }
): ReferenceName | undefined {
  if (missing) {
    return { mention: undefined, name: undefined };
  }
  if (title === undefined && !known) {
    return undefined;
  }
  return {
    mention: isPartyRoute(route) ? mentionDrawn({ known, title }, shortForm(route)) : undefined,
    name: title,
  };
}

/** How a read tells the projection what one reference resolved to. */
type NameReport = (address: string, name: ReferenceName | undefined) => void;

/** One reference's own read, which draws nothing: it reads the target the painted link reads,
 *  by the same query (`useReferenceTarget`), so a line of text and a link to the same target make
 *  one request between them and never name it two ways. */
function OwnNameRead({
  address,
  onName,
  route,
}: {
  address: string;
  onName: NameReport;
  route: DispatchReferenceRoute;
}): ReactNode {
  const target = useReferenceTarget(route, { text: false });
  const { known, missing } = target;
  // The projection names an address standing in a line: a deleted post by the deleted line, an
  // edited one with its mark, as a link to it is named (`useReferenceLabel`).
  const { text: title } = useReferenceLabel(route, target);
  const answer = referenceAnswer(route, { known, missing, title });
  const name = answer?.name;
  const mention = answer?.mention;
  const answered = answer !== undefined;
  useEffect(() => {
    onName(address, answered ? { mention, name } : undefined);
  }, [address, answered, mention, name, onName]);
  return null;
}

/**
 * What the page published answers for one reference (`reference-names.tsx`), by the one rule R8
 * is everywhere:
 *
 * - **A name the page's batched read gave it**: that name, which an issue's key is written before
 *   as a link to an issue reads (`standingName`).
 * - **Gone, or one the reader may not see, and ambiguous**: `null`, the words its author wrote,
 *   and no read of its own.
 * - **Asked for and not answered yet**, or answered with a reading the store has not projected
 *   yet: `null` too, since the page's read is the one that names it.
 * - **Answered with no words to name it by**: `undefined`, which is no answer that the target has
 *   no name, so it is read on its own, as a link to it is (`RefLink`).
 *
 * `undefined` too where the page holds nothing for the reference. A party is never asked of the
 * page's batched read, so it is always read on its own.
 */
function publishedAnswer(
  address: string,
  route: DispatchReferenceRoute,
  known: KnownReferences
): ReferenceName | null | undefined {
  if (isPartyRoute(route)) return undefined;
  const name = known.names.get(address);
  if (name !== undefined) return { mention: undefined, name };
  if (known.unnamed.has(address)) return undefined;
  return known.ambiguous.has(address) || known.gone.has(address) || known.asked.has(address)
    ? null
    : undefined;
}

/** What a line's own reads answer it, and what the page published, read as one lookup. */
interface NameLookup {
  readonly known: KnownReferences;
  readonly names: ReferenceNameLookup;
  readonly onName: NameReport;
}

/** The names a line reads its references by (`useReferenceNames`): a view over what the page
 *  published and what the line's own reads answered, made anew only where either changes, which
 *  copies neither. Each reference is looked up in what the page published once for each view,
 *  however often the projection asks for it. The page's answer stands over a read of one's own:
 *  a reference the page names now, or waits on its read for, is the page's alone, whatever an
 *  earlier read of its own answered. */
function useNameLookup(): NameLookup {
  const known = useKnownReferences();
  const [read, setRead] = useState<ReadonlyMap<string, ReferenceName>>(NO_NAMES);
  const onName = useCallback<NameReport>((address, name) => {
    setRead((current) => {
      const now = current.get(address);
      if (
        now?.name === name?.name &&
        now?.mention === name?.mention &&
        current.has(address) === (name !== undefined)
      ) {
        return current;
      }
      const next = new Map(current);
      if (name === undefined) {
        next.delete(address);
      } else {
        next.set(address, name);
      }
      return next;
    });
  }, []);
  const names = useMemo<ReferenceNameLookup>(() => {
    const published = new Map<string, ReferenceName | null | undefined>();
    return {
      get: (reference) => {
        if (!published.has(reference)) {
          const route = parseNodeReference(reference);
          published.set(
            reference,
            route === undefined ? undefined : publishedAnswer(reference, route, known)
          );
        }
        const answer = published.get(reference);
        return answer === undefined ? read.get(reference) : (answer ?? undefined);
      },
    };
  }, [known, read]);
  return { known, names, onName };
}

/** The own reads of every reference among `addresses` the page answers nothing for
 *  (`publishedAnswer`), mounted in what this answers, which draws nothing and which the caller
 *  mounts wherever it draws. Each read takes its role by `issue`, the issue the record holding
 *  the addresses is on (`RecordIssue`). The reads are keyed on the references themselves, never
 *  on the array: a caller answers a fresh one every time it runs, and reading the same
 *  references again for it would never settle. */
function useOwnNameReads(
  addresses: readonly string[],
  issue: string | undefined,
  { known, onName }: NameLookup
): ReactNode {
  const addressKey = addresses.join("\n");
  const unread = useMemo(() => {
    const own = new Set<string>();
    for (const address of addressKey === "" ? [] : addressKey.split("\n")) {
      const route = parseNodeReference(address);
      if (route !== undefined && publishedAnswer(address, route, known) === undefined) {
        own.add(address);
      }
    }
    return [...own].join("\n");
  }, [addressKey, known]);
  return useMemo(
    () => (
      // The host every read sits in: an element that can hold no markup, so a body drawn under
      // one draws its characters rather than a link.
      <ReferenceHostIs host="plain">
        <RecordIssue.Provider value={issue}>
          {(unread === "" ? [] : unread.split("\n")).map((address) => {
            const route = parseNodeReference(address);
            return route === undefined ? null : (
              <OwnNameRead address={address} key={address} onName={onName} route={route} />
            );
          })}
        </RecordIssue.Provider>
      </ReferenceHostIs>
    ),
    [issue, onName, unread]
  );
}

/**
 * What each of `addresses` reads as (`ReferenceName`), by the one rule every plain line and every
 * row of the new issue page names a reference by: what the page already read for it, where a
 * page published it (`publishedAnswer`), and otherwise the target's own read
 * (`useReferenceTarget`), mounted in `reads`, which draws nothing and which the caller mounts
 * wherever it draws. A reference with no name - the page waits on its read for it, Dispatch
 * answered it has none, or its own read has not answered - is left out, so it reads as the words
 * its author wrote (R8). A read's name for an address the caller has since moved off is kept
 * rather than dropped, since the same reference again names the same thing. `issue` is the issue
 * the record holding the addresses is on, by which a role is read (`RecordIssue`).
 */
export function useReferenceNames(
  addresses: readonly string[],
  issue: string | undefined
): { names: ReferenceNameLookup; reads: ReactNode } {
  const lookup = useNameLookup();
  const reads = useOwnNameReads(addresses, issue, lookup);
  return { names: lookup.names, reads };
}

/** How a caller of `usePlainHeadline` asks for its line. */
export interface PlainHeadlineOptions {
  /** Reads the text's first line alone rather than the whole of it (`useMarkdownHeadline`). */
  readonly firstLine?: boolean;
  /** The issue the record holding the text is on, by which a role in it is read (`RecordIssue`),
   *  for a caller that names no `source`: a line that reads no question map, as a row of the new
   *  issue page reads none (`node-row-words`). A `source` names its own issue. */
  readonly issue?: string | undefined;
  /** The most characters the line holds; the whole text where it is left out. */
  readonly max?: number;
  /** The record the text is, whose document's question map names a `#id` in it, as a rendered
   *  body of the same record names one (`useTextReferences`); a line of no named record reads a
   *  `#id` as written. */
  readonly source?: TextSource;
  /** The row of the reading list this line is drawn on (`DISPATCH_READING_SURFACES`). The
   *  projection reads nothing of it: it names the surface a mount draws, which is how the list's
   *  unit test ties each mount to its row (`surface-lists.test.ts`). */
  readonly surface?: DispatchReadingSurface;
}

/**
 * One line of plain text for a place that can hold no markup - an `<option>`, a `<title>`, an
 * `aria-label` - read by the one reader every rendered body reads (`useMarkdownHeadline`), so
 * what a reader hears of a record and what they see of it are the same words. There is no second
 * stripper: formatting goes and no syntax stays, and every reference - a `dispatch://` address,
 * a dashboard address, a short id, an issue key, and a `#id` where the caller names the text's
 * `source` - reads as the painted line draws it: its target's current name, and the words its
 * author wrote where Dispatch answered that it has none (R8).
 *
 * The projection says which references it read (`MarkdownHeadline.addresses`), and each is named
 * by the one rule `useReferenceNames` names one by: from what the page already read where a page
 * published it, and otherwise by its own read - and it reads the question map of `source`'s
 * document where the line holds a `#id`, the read a rendered body of the same record makes - and
 * the projection runs again with them: two passes over a line that holds a reference, one over
 * the rest. Nothing is set while the line is drawn: what names the references is a view over the
 * page's answer and the reads' own (`names`), which the projection reads when it runs, and each
 * read starts in the commit after the one whose projection found its reference. A name read for
 * a source this one replaced is kept rather than dropped, since the same reference in the next
 * source names the same thing. Where the caller hands over the text's `markdown`, beside its
 * tree or alone, the projection names only what that source cites (`citedReferences`), as a
 * rendered body links only that: the `#q2` an escaped `\#q2` leaves in the words stays those
 * words.
 *
 * The words are `tree`'s, the reading Go made of a stored text, where the caller has one, and
 * the parse of `markdown` for a text Dispatch does not store (`useMarkdownHeadline`): a line
 * read as plain words is read from the same document the surface draws.
 *
 * `text` is `undefined` until the engine has answered, which is one pre-paint commit on a page
 * whose engine is loaded; it says which of its references it wrote a name for
 * (`MarkdownHeadline.named`). `reads` draws nothing and the caller mounts it wherever it draws
 * the line.
 */
export function usePlainHeadline(
  tree: DisplayTree | undefined,
  markdown: string | undefined,
  { firstLine = false, issue, max = Number.POSITIVE_INFINITY, source }: PlainHeadlineOptions = {}
): { reads: ReactNode; text: MarkdownHeadline | undefined } {
  const lookup = useNameLookup();
  const [asksQuestions, setAsksQuestions] = useState(false);
  // Asked once the projection says the line holds a `#id`, which is after it mounted
  // (`useReferenceQuery`).
  const questionRead = useReferenceQuery(
    asksQuestions && source !== undefined ? questionsQuery(source) : undefined
  );
  const questions = useMemo(() => questionReferences(questionRead?.data), [questionRead?.data]);
  const cited = useMemo(
    () => (markdown === undefined ? undefined : citedReferences(markdown, questions)),
    [markdown, questions]
  );
  const projects = useProjectKeys();
  const names = lookup.names;
  const naming = useMemo<ReferenceNaming>(
    () => ({ cited, names, projects, questions }),
    [cited, names, projects, questions]
  );
  const text = useMarkdownHeadline(tree, max, markdown, naming, firstLine);
  const holdsQuestions = text?.questions === true;
  // In the layout phase of the commit whose projection found the `#id`, before it is painted.
  useLayoutEffect(() => {
    if (holdsQuestions) setAsksQuestions(true);
  }, [holdsQuestions]);
  const reads = useOwnNameReads(text?.addresses ?? NO_ADDRESSES, source?.issue ?? issue, lookup);
  return { reads, text };
}

/**
 * `usePlainHeadline` for a caller whose element can hold no children of its own - a checkbox
 * named after the row it marks, a button whose label is its row's subject: the reads sit beside
 * whatever `children` draws rather than inside it.
 */
export function PlainText({
  children,
  markdown,
  max = Number.POSITIVE_INFINITY,
  source,
  surface,
  tree,
}: {
  /** Draws whatever holds the line: the control the label names, the element the title is on.
   *  `undefined` until the engine has answered. */
  children: (text: string | undefined) => ReactNode;
  markdown: string | undefined;
  max?: number;
  /** The record the text is (`PlainHeadlineOptions`). */
  source?: TextSource;
  /** The row of the reading list this line is drawn on (`PlainHeadlineOptions`). */
  surface?: DispatchReadingSurface;
  /** The reading Go made of this stored text, whose words the line is; absent for a text
   *  Dispatch does not store, whose `markdown` is parsed instead. */
  tree?: DisplayTree;
}): ReactNode {
  const { reads, text } = usePlainHeadline(tree, markdown, { max, source, surface });
  return (
    <>
      {reads}
      {children(text?.text)}
    </>
  );
}
