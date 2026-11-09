import { type ReactNode, useCallback, useEffect, useMemo, useState } from "react";

import {
  type MarkdownHeadline,
  NO_REFERENCE_NAMES,
  type ReferenceNames,
  useMarkdownHeadline,
} from "./markdown-engine";
import { ReferenceHostIs } from "./reference-host";
import { useReferenceTarget } from "./reference-target";
import { type DispatchReferenceRoute, parseDispatchReference } from "./routes";

/** What a reference reads as in plain words: the name the painted link reads for it
 *  (`RefLink`), which for an issue is its key and then its title, as a project's issue list
 *  writes a key beside one. Undefined while the target has not resolved, and for one the reader
 *  may not see, where the projection keeps the reference's short form. */
function referenceName(
  route: DispatchReferenceRoute,
  title: string | undefined
): string | undefined {
  if (title === undefined) {
    return undefined;
  }
  return route.kind === "issue" ? `${route.key} ${title}` : title;
}

/** How a read tells the projection what one address resolved to. */
type NameReport = (address: string, name: string | undefined) => void;

/** One address's own read, which draws nothing: it reads the target the painted link reads
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
  const { title } = useReferenceTarget(route, { text: false });
  const name = referenceName(route, title);
  useEffect(() => {
    onName(address, name);
  }, [address, name, onName]);
  return null;
}

/** The headline of a source the engine has not parsed: no address to read, and one identity, so
 *  the reads below never re-run for a fresh empty array. */
const NO_ADDRESSES: readonly string[] = [];

/**
 * One line of plain text for a place that can hold no markup - an `<option>`, a `<title>`, an
 * `aria-label` - read by the one reader every rendered body reads (`useMarkdownHeadline`), so
 * what a reader hears of a record and what they see of it are the same words. There is no second
 * stripper: formatting goes and no syntax stays, and every `dispatch://` address reads as its
 * target's current name - the same name the painted link reads - falling back to its short form
 * until that name resolves and for a target the reader may not see.
 *
 * The projection says which addresses it drew, this reads each one's name, and the projection
 * runs again with them: two passes over a line that holds a reference, one over the rest. A name
 * read for a source this one replaced is kept rather than dropped, since the same address in the
 * next source names the same thing.
 *
 * `text` is `undefined` until the engine has answered, which is one pre-paint commit on a page
 * whose engine is loaded. `reads` draws nothing and the caller mounts it wherever it draws the
 * line.
 */
export function usePlainHeadline(
  markdown: string | undefined,
  max = Number.POSITIVE_INFINITY
): { reads: ReactNode; text: MarkdownHeadline | undefined } {
  const [names, setNames] = useState<ReferenceNames>(NO_REFERENCE_NAMES);
  const text = useMarkdownHeadline(markdown, max, names);
  // Keyed on the addresses themselves, never on the array: the projection answers a fresh one
  // every time it runs, and re-reading the same addresses for it would never settle.
  const addressKey = (text?.addresses ?? NO_ADDRESSES).join("\n");
  const routes = useMemo(() => {
    const found = new Map<string, DispatchReferenceRoute>();
    for (const address of addressKey === "" ? [] : addressKey.split("\n")) {
      const route = parseDispatchReference(address);
      if (route !== undefined) {
        found.set(address, route);
      }
    }
    return found;
  }, [addressKey]);
  const onName = useCallback<NameReport>((address, name) => {
    setNames((current) => {
      if (name === undefined ? !current.has(address) : current.get(address) === name) {
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
  const reads = useMemo(
    () => (
      // The host every read sits in: an element that can hold no markup, so a body drawn under
      // one draws its characters rather than a link.
      <ReferenceHostIs host="plain">
        {[...routes].map(([address, route]) => (
          <ReferenceNameRead address={address} key={address} onName={onName} route={route} />
        ))}
      </ReferenceHostIs>
    ),
    [onName, routes]
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
  markdown,
  max = Number.POSITIVE_INFINITY,
}: {
  /** Draws whatever holds the line: the control the label names, the element the title is on.
   *  `undefined` until the engine has answered. */
  children: (text: string | undefined) => ReactNode;
  markdown: string | undefined;
  max?: number;
}): ReactNode {
  const { reads, text } = usePlainHeadline(markdown, max);
  return (
    <>
      {reads}
      {children(text?.text)}
    </>
  );
}
