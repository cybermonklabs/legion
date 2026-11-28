import { createContext, type ReactNode, useContext } from "react";

/**
 * The shape of the host a stored record is drawn in, which is the one thing that says whether a
 * reference drawn in it is a live link. A host can be one of four shapes and no fifth, and each
 * sets this once for everything under it, so no call site passes a prop guessing at it:
 *
 * - `record` — a record a reader reads: a reference is a link and a click follows it.
 * - `control` — the inside of a link or a button: the host is already the one control a click
 *   belongs to, so a reference reads as a link and is none. No link inside a link.
 * - `plain` — an element that can hold no markup: an `<option>`, a `<title>`, an `aria-label`.
 *   Its text is the plain-text projection (`PlainText`), where a reference is its target's name
 *   and nothing is a link.
 * - `diff` — a suggestion's replacement, which shows the exact characters it swaps: no title is
 *   drawn over a reference there, since the point of the line is what it would write.
 */
export type ReferenceHost = "record" | "control" | "plain" | "diff";

/** A reader reads a record unless a host says otherwise, which is what almost every page is. */
const ReferenceHostContext = createContext<ReferenceHost>("record");

/** The shape of the host the caller is drawn in. */
export function useReferenceHost(): ReferenceHost {
  return useContext(ReferenceHostContext);
}

/** Whether a reference drawn in `host` is a link a click follows. */
export function hostLinksReferences(host: ReferenceHost): boolean {
  return host === "record";
}

/** Whether `host` draws a reference at all - its title, its hover card and the cards under a
 *  body that is nothing but references. A host that can hold no markup, and a diff, draw the
 *  characters their text holds instead. */
export function hostDrawsMarkup(host: ReferenceHost): boolean {
  return host === "record" || host === "control";
}

/** Declares the shape of the host everything inside is drawn in. A host nests: the body of a
 *  card inside a link is a `control` whatever the page around the card is. */
export function ReferenceHostIs({
  children,
  host,
}: {
  children: ReactNode;
  host: ReferenceHost;
}): ReactNode {
  return <ReferenceHostContext.Provider value={host}>{children}</ReferenceHostContext.Provider>;
}
