import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Link } from "react-router-dom";

import {
  borderDefault,
  card,
  linkHoverText,
  linkText,
  liveDotBg,
  skeletonBg,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useAgentText } from "../refs/AgentName";
import { type DefinitionPlace, defineWords, readDefinition } from "./ask-rewrite";

/** How often the popover reads the definition again while the agent's answer is on its way, and
 *  for how long: the side question waits 60 seconds for its answer. */
const AGENT_POLL_MS = 1_000;
const AGENT_WAIT_MS = 60_000;
const WIDTH = 340;
const GAP = 6;
/** A selection that starts this close to its column's left edge opens the popover on that edge,
 *  so no sliver of the text it covers shows to its left. */
const SNAP = 120;

/** Where a selection stands: its words, its box, and the box of the text column it is in (the
 *  root's content box, inside its padding and border), all in viewport coordinates. */
export interface SelectedWords {
  words: string;
  rect: DOMRect;
  column: { left: number; right: number };
}

/** Words selected under `root`, with where they stand on screen, or null when the selection is
 *  empty or reaches outside `root`. */
export function selectedWordsIn(root: HTMLElement | null): SelectedWords | null {
  const selection = window.getSelection();
  if (root === null || selection === null || selection.rangeCount === 0 || selection.isCollapsed)
    return null;
  const range = selection.getRangeAt(0);
  if (!root.contains(range.commonAncestorContainer)) return null;
  const words = selection.toString().replace(/\s+/g, " ").trim();
  if (words === "") return null;
  const box = root.getBoundingClientRect();
  const style = getComputedStyle(root);
  const inset = (side: "Left" | "Right") =>
    Number.parseFloat(style[`padding${side}`]) + Number.parseFloat(style[`border${side}Width`]);
  return {
    words,
    rect: range.getBoundingClientRect(),
    column: { left: box.left + inset("Left"), right: box.right - inset("Right") },
  };
}

/**
 * Define's answer, beside the selected words: what they mean where they stand, read from
 * Dispatch's record, with a link to the place in the documents that defines them, and below it,
 * for words in an ask, the asking agent's own answer under its name once it comes. The agent's
 * name is the one label: it tells the agent's words from the record's.
 *
 * It takes no focus, so the selection stays drawn under it, and it closes on Escape, on a
 * press anywhere outside it, and when the page scrolls the words away.
 */
export function DefinePopover({
  column,
  onClose,
  place,
  rect,
  words,
}: {
  /** The text column the words are in: the popover keeps inside it where it can. */
  column: SelectedWords["column"];
  onClose: () => void;
  place: DefinitionPlace;
  /** The selection's box when Define was pressed, in viewport coordinates. */
  rect: DOMRect;
  words: string;
}): ReactNode {
  const panelRef = useRef<HTMLDivElement>(null);
  const agentText = useAgentText();
  const [openedAt] = useState(() => Date.now());
  const first = useQuery({
    queryKey: ["definition", words, place],
    queryFn: () => defineWords(words, place),
    staleTime: Number.POSITIVE_INFINITY,
  });
  const asking = first.data?.agent.state === "asking";
  const later = useQuery({
    enabled: asking,
    queryKey: ["definition", first.data?.id],
    queryFn: () => readDefinition(first.data?.id ?? ""),
    refetchInterval: (query) =>
      query.state.data?.agent.state === "asking" && Date.now() - openedAt < AGENT_WAIT_MS
        ? AGENT_POLL_MS
        : false,
  });
  const definition = later.data ?? first.data;
  const agent = definition?.agent;

  const [position, setPosition] = useState({ left: rect.left, top: rect.bottom + GAP });
  useLayoutEffect(() => {
    const height = panelRef.current?.offsetHeight ?? 0;
    const width = Math.min(WIDTH, window.innerWidth - 32);
    // Inside the words' own column where it is wide enough, else inside the page's 16 px gutter.
    const low = Math.max(16, column.left);
    const high = Math.min(window.innerWidth - 16, column.right) - width;
    const left =
      high < low
        ? Math.max(16, Math.min(rect.left, window.innerWidth - width - 16))
        : rect.left - low < SNAP
          ? low
          : Math.min(rect.left, high);
    const below = rect.bottom + GAP;
    const top =
      below + height > window.innerHeight - 8 && rect.top - GAP - height >= 8
        ? rect.top - GAP - height
        : below;
    setPosition((current) =>
      current.left === left && current.top === top ? current : { left, top }
    );
  });

  const close = useRef(onClose);
  close.current = onClose;
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      event.preventDefault();
      event.stopPropagation();
      close.current();
    };
    const onPointer = (event: PointerEvent) => {
      if (event.target instanceof Node && panelRef.current?.contains(event.target)) return;
      close.current();
    };
    const onScroll = (event: Event) => {
      if (event.target instanceof Node && panelRef.current?.contains(event.target)) return;
      close.current();
    };
    document.addEventListener("keydown", onKey, true);
    document.addEventListener("pointerdown", onPointer, true);
    window.addEventListener("scroll", onScroll, true);
    return () => {
      document.removeEventListener("keydown", onKey, true);
      document.removeEventListener("pointerdown", onPointer, true);
      window.removeEventListener("scroll", onScroll, true);
    };
  }, []);

  const prose = `text-[13px] leading-5 ${textSecondaryOnSurface}`;
  return createPortal(
    <div
      aria-label={`What “${words}” means`}
      className={`fixed z-50 rounded-xl border p-3 shadow-lg ${card} ${borderDefault}`}
      data-define-popover=""
      ref={panelRef}
      role="dialog"
      style={{
        left: position.left,
        top: position.top,
        width: Math.min(WIDTH, window.innerWidth - 32),
      }}
    >
      {definition === undefined ? (
        first.isError ? (
          <p className={prose}>Dispatch could not define these words. Press d to try again.</p>
        ) : (
          <div aria-label="Defining" className="space-y-1.5 py-0.5" role="status">
            <div className={`h-3 w-full animate-pulse rounded ${skeletonBg}`} />
            <div className={`h-3 w-4/5 animate-pulse rounded ${skeletonBg}`} />
          </div>
        )
      ) : (
        <>
          <p className={prose}>{definition.meaning}</p>
          {definition.link === undefined ? null : (
            <Link
              className={`mt-1.5 inline-block text-xs font-medium ${linkText} ${linkHoverText}`}
              onClick={onClose}
              to={definition.link.href}
            >
              {definition.link.label} →
            </Link>
          )}
        </>
      )}
      {agent === undefined || agent.state === "none" || agent.actor === undefined ? null : (
        <div className={`mt-2.5 border-t pt-2.5 ${borderDefault}`} data-define-agent={agent.state}>
          {/* The agent's name is the one label: it tells its words from the record's. */}
          <p className={`flex items-center gap-1.5 text-xs font-medium ${textMutedOnSurface}`}>
            <span aria-hidden="true" className={`size-1.5 shrink-0 rounded-full ${liveDotBg}`} />
            {agentText.label(agent.actor)}
          </p>
          {agent.state === "answered" ? (
            <p className={`mt-1 ${prose}`}>{agent.answer}</p>
          ) : (
            <span aria-label="Asking the agent" className="mt-2 flex gap-1" role="status">
              {[0, 200, 400].map((delay) => (
                <span
                  className={`size-1.5 animate-pulse rounded-full bg-current ${textMutedOnSurface}`}
                  key={delay}
                  style={{ animationDelay: `${delay}ms` }}
                />
              ))}
            </span>
          )}
        </div>
      )}
    </div>,
    document.body
  );
}
