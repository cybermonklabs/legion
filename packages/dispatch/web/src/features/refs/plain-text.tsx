import { type DispatchReadingSurface, mentionDrawn } from "@legion/contracts";
import { type ReactNode, useCallback, useEffect, useMemo, useState } from "react";

import type { DisplayTree } from "../../api/types";
import {
  citedReferences,
  type MarkdownHeadline,
  type ReferenceName,
  type ReferenceNaming,
  useMarkdownHeadline,
} from "./markdown-engine";
import { isPartyRoute } from "./party-references";
import { useProjectKeys } from "./project-keys";
import { ReferenceHostIs } from "./reference-host";
import {
  questionReferences,
  questionsQuery,
  useReferenceQuery,
  useReferenceTarget,
} from "./reference-target";
import { RecordIssue } from "./role-holders";
import { type DispatchReferenceRoute, parseNodeReference, shortForm } from "./routes";
import type { TextSource } from "./text-references";

/** No reference read yet, shared so a line that names none keeps one identity across renders. */
const NO_NAMES: ReadonlyMap<string, ReferenceName> = new Map();

/** What a reference reads as in plain words once its target has answered (`ReferenceName`): the
 *  name the painted link reads for it (`RefLink`), which for an issue is its key and then its
 *  title, as a project's issue list writes a key beside one, and for a party's mention what the
 *  painted mention draws (`@legion/contracts`'s `mentionDrawn`): the mention sign and that name,
 *  or the sign and the party's short form where it exists with no name. A target Dispatch
 *  answered does not exist, or exists with no name of its own, has no name, and reads as the
 *  words its author wrote (R8). Undefined while the target has not answered, and for one the
 *  reader may not see, where the projection keeps the words written, as the painted link does
 *  until its title resolves. */
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
    name: title === undefined || route.kind !== "issue" ? title : `${route.key} ${title}`,
  };
}

/** How a read tells the projection what one reference resolved to. */
type NameReport = (address: string, name: ReferenceName | undefined) => void;

/** One reference's own read, which draws nothing: it reads the target the painted link reads
 *  (`useReferenceTarget`), so a line of text and the same line as a label never name the same
 *  reference two ways. */
function ReferenceNameRead({
  address,
  onName,
  route,
}: {
  address: string;
  onName: NameReport;
  route: DispatchReferenceRoute;
}): ReactNode {
  const { known, missing, title } = useReferenceTarget(route, { text: false });
  const answer = referenceAnswer(route, { known, missing, title });
  const name = answer?.name;
  const mention = answer?.mention;
  const answered = answer !== undefined;
  useEffect(() => {
    onName(address, answered ? { mention, name } : undefined);
  }, [address, answered, mention, name, onName]);
  return null;
}

/** How a caller of `usePlainHeadline` asks for its line. */
export interface PlainHeadlineOptions {
  /** Reads the text's first line alone rather than the whole of it (`useMarkdownHeadline`). */
  readonly firstLine?: boolean;
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
 * The projection says which references it read, this reads each one's name - and the question
 * map of `source`'s document where the line holds a `#id`, the read a rendered body of the same
 * record makes - and the projection runs again with them: two passes over a line that holds a
 * reference, one over the rest. A name read for a source this one replaced is kept rather than
 * dropped, since the same reference in the next source names the same thing. Where the caller
 * hands over the text's `markdown`, beside its tree or alone, the projection names only what
 * that source cites (`citedReferences`), as a rendered body links only that: the `#q2` an
 * escaped `\#q2` leaves in the words stays those words.
 *
 * The words are `tree`'s, the reading Go made of a stored text, where the caller has one, and
 * the parse of `markdown` for a text Dispatch does not store (`useMarkdownHeadline`): a line
 * read as plain words is read from the same document the surface draws.
 *
 * `text` is `undefined` until the engine has answered, which is one pre-paint commit on a page
 * whose engine is loaded. `reads` draws nothing and the caller mounts it wherever it draws the
 * line.
 */
export function usePlainHeadline(
  tree: DisplayTree | undefined,
  markdown: string | undefined,
  { firstLine = false, max = Number.POSITIVE_INFINITY, source }: PlainHeadlineOptions = {}
): { reads: ReactNode; text: MarkdownHeadline | undefined } {
  const [names, setNames] = useState<ReadonlyMap<string, ReferenceName>>(NO_NAMES);
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
  const naming = useMemo<ReferenceNaming>(
    () => ({ cited, names, projects, questions }),
    [cited, names, projects, questions]
  );
  const text = useMarkdownHeadline(tree, max, markdown, naming, firstLine);
  const holdsQuestions = text?.questions === true;
  if (holdsQuestions && !asksQuestions) setAsksQuestions(true);
  // Keyed on the references themselves, never on the array: the projection answers a fresh one
  // every time it runs, and re-reading the same references for it would never settle. A source
  // the engine has not parsed has no reference at all, which is the empty key.
  const addressKey = (text?.addresses ?? []).join("\n");
  const routes = useMemo(() => {
    const found = new Map<string, DispatchReferenceRoute>();
    for (const address of addressKey === "" ? [] : addressKey.split("\n")) {
      const route = parseNodeReference(address);
      if (route !== undefined) {
        found.set(address, route);
      }
    }
    return found;
  }, [addressKey]);
  const onName = useCallback<NameReport>((address, name) => {
    setNames((current) => {
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
  const issue = source?.issue;
  const reads = useMemo(
    () => (
      // The host every read sits in: an element that can hold no markup, so a body drawn under
      // one draws its characters rather than a link. Each read takes its role by the issue the
      // record is on, as the painted line's links do (`RecordIssue`).
      <ReferenceHostIs host="plain">
        <RecordIssue.Provider value={issue}>
          {[...routes].map(([address, route]) => (
            <ReferenceNameRead address={address} key={address} onName={onName} route={route} />
          ))}
        </RecordIssue.Provider>
      </ReferenceHostIs>
    ),
    [issue, onName, routes]
  );
  return { reads, text };
}

/**
 * `usePlainHeadline` for a caller whose element can hold no children of its own - a checkbox
 * named after the row it marks, a button whose label is its row's subject: the reads sit beside
 * whatever `children` draws rather than inside it.
 */
export function PlainText({
  children,
  firstLine = false,
  markdown,
  max = Number.POSITIVE_INFINITY,
  source,
  surface,
  tree,
}: {
  /** Draws whatever holds the line: the control the label names, the element the title is on.
   *  `undefined` until the engine has answered. */
  children: (text: string | undefined) => ReactNode;
  /** Reads the text's first line alone rather than the whole of it (`useMarkdownHeadline`):
   *  a closed row of the priorities table, which shows the first line of a description and no
   *  more of it, is the one caller that asks. */
  firstLine?: boolean;
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
  const { reads, text } = usePlainHeadline(tree, markdown, { firstLine, max, source, surface });
  return (
    <>
      {reads}
      {children(text?.text)}
    </>
  );
}
