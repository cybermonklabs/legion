import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";

import { NEAR_BAND } from "./clamp-measurer";
import { HandoffBodyLoader } from "./handoff-body-loader";

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
export interface HandoffBodyReads {
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
): { readonly band: HandoffBand; readonly value: HandoffBodyReads } {
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
