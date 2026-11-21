import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { Definition } from "../../api/types";
import { useTooltips } from "../../components/tooltips";
import {
  borderDefault,
  card,
  linkHoverText,
  linkText,
  liveDotBg,
  pageHairBg,
  pageHairBorder,
  pageInkText,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
  pageSegmentDoneBg,
  skeletonBg,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useAgentText } from "../refs/AgentName";
import {
  buildReferencePath,
  type DispatchReferenceRoute,
  parseNodeReference,
  shortForm,
} from "../refs/routes";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import type { DefinitionPlace } from "./ask-rewrite";
import { citationLabel } from "./citation-label";

/**
 * Each colour the popover draws inside `NoTooltips`, the new issue page's, by the role its colour
 * has on today's pages: the popover is the page's paper inside its hairline rule, with a hairline
 * over the agent's answer; the meaning and the agent's answer are the muted grey, what qualifies
 * them - the agent's name, why Dispatch could not define the words, the dots while the agent is
 * asked - the quiet grey, a link the ink with its underline, and the lines drawn while the words
 * are defined the hairline grey. The agent's dot, a mark that draws no words, is the running
 * mark `AgentName` draws there (`pageSegmentDoneBg`). A link's words, a reason and an answer are
 * values there, which break anywhere inside the popover's width.
 */
const ON_PAGE = {
  agentRule: pageHairBorder,
  frame: `${pageHairBorder} ${pagePaperBg}`,
  link: `${pageInkText} underline max-w-full [overflow-wrap:anywhere]`,
  prose: `${pageMutedText} max-w-full [overflow-wrap:anywhere]`,
  quiet: pageQuietText,
  skeleton: pageHairBg,
} as const;

/** How often the popover reads the definition again while the agent's answer is on its way, and
 *  for how long at most: the side question waits 60 seconds for its answer (`agent.until`). */
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

/** Whether the asking session's answer may still come: its side question waits, and its time
 *  (`until`, else 60 seconds from the press) has not passed by more than one more read. */
function agentAnswerDue(definition: Definition | undefined, openedAt: number): boolean {
  if (definition?.agent.state !== "waiting") return false;
  const until = definition.agent.until;
  const deadline = until === undefined ? openedAt + AGENT_WAIT_MS : Date.parse(until);
  return Date.now() <= deadline + AGENT_POLL_MS;
}

/** The text of a document a citation names, with its blocks: an issue's spec by the issue's
 *  document, or another document of the issue by its slug. Undefined for any other place. */
async function citedText(route: DispatchReferenceRoute) {
  if (route.kind === "spec") {
    const issue = await api.getIssue(route.key);
    return api.getArtifactTextWithBlocks(issue.primary_artifact_id);
  }
  if (route.kind === "artifact" && "key" in route) {
    return api.getIssueArtifactTextWithBlocks(route.key, route.slug);
  }
  return undefined;
}

/** How Define's citation reads (INFRA-3231's R7): the document, then the block it cites by what
 *  the block carries (`citationLabel`), "PANE-5 spec · R3"; the document alone where the
 *  citation names no block, and the reference's own short form until the document is read. */
function useCitationName(route: DispatchReferenceRoute): string {
  const blockId = "block" in route ? route.block?.id : undefined;
  const text = useQuery({
    enabled: blockId !== undefined && (route.kind === "spec" || route.kind === "artifact"),
    queryFn: () => citedText(route),
    queryKey: ["define-citation", buildReferencePath(route)],
  }).data;
  const document = shortForm({ ...route, block: undefined } as DispatchReferenceRoute);
  if (blockId === undefined) return document;
  if (text === undefined) return shortForm(route);
  return `${document} · ${citationLabel(text.markdown, text.blocks ?? [], blockId)}`;
}

/** A link of the record to the place that defines the words: a `dispatch://` reference opens
 *  that place in the app, named as `useCitationName` names it; a web address opens in a tab of
 *  its own, and anything else is no link. */
function DefinitionLink({ href, onFollow }: { href: string; onFollow: () => void }): ReactNode {
  const route = parseNodeReference(href);
  return route === undefined ? (
    <WebDefinitionLink href={href} />
  ) : (
    <ReferenceDefinitionLink onFollow={onFollow} route={route} />
  );
}

const definitionLinkShape = "mt-1.5 mr-3 inline-block text-xs font-medium";

function ReferenceDefinitionLink({
  route,
  onFollow,
}: {
  route: DispatchReferenceRoute;
  onFollow: () => void;
}): ReactNode {
  const tooltips = useTooltips();
  const name = useCitationName(route);
  return (
    <Link
      className={`${definitionLinkShape} ${tooltips ? `${linkText} ${linkHoverText}` : ON_PAGE.link}`}
      onClick={onFollow}
      to={buildReferencePath(route)}
    >
      {name} →
    </Link>
  );
}

function WebDefinitionLink({ href }: { href: string }): ReactNode {
  const tooltips = useTooltips();
  if (!/^https?:\/\//.test(href)) return null;
  return (
    <a
      className={`${definitionLinkShape} ${tooltips ? `${linkText} ${linkHoverText}` : ON_PAGE.link}`}
      href={href}
      rel="noreferrer"
      target="_blank"
    >
      {href} →
    </a>
  );
}

/**
 * Define's answer, beside the selected words: what they mean where they stand, read from
 * Dispatch's record (`POST /api/v1/definitions`), with a link to the place in the documents that
 * defines them, and below it, for words in an ask, the asking agent's own answer under its name
 * once it comes (`GET /api/v1/definitions/{id}`, read again while it may). The agent's name is the
 * one label: it tells the agent's words from the record's.
 *
 * Each press of Define is one definition (a new `rect`, the selection's box as it was pressed),
 * which Dispatch records as the reader's question about those words. The popover takes no focus,
 * so the selection stays drawn under it, and it closes on Escape, on a press anywhere outside
 * it, and when the page scrolls the words away.
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
  const [openedAt, setOpenedAt] = useState(() => Date.now());
  // The popover says why a definition was refused itself, so the write is inline.
  const define = useKeyedWrite({
    inline: true,
    label: () => `Definition of “${words}”`,
    method: "POST",
    write: (_variables, bounds) => api.defineWords({ ...place, words: [words] }, bounds),
  });
  const { mutate, reset } = define;
  // One definition for each press: the press is what draws a new box, and a render of the same
  // press - React's second run of an effect in development among them - defines nothing again.
  const definedFor = useRef<DOMRect | null>(null);
  useEffect(() => {
    if (definedFor.current === rect) return;
    definedFor.current = rect;
    setOpenedAt(Date.now());
    reset();
    mutate();
  }, [mutate, rect, reset]);
  const first = define.data ?? undefined;
  const later = useQuery({
    enabled: first !== undefined && agentAnswerDue(first, openedAt),
    queryKey: ["definition", first?.id],
    queryFn: () => api.getDefinition(first?.id ?? ""),
    refetchInterval: (query) =>
      agentAnswerDue(query.state.data ?? first, openedAt) ? AGENT_POLL_MS : false,
  });
  const definition = later.data !== undefined && later.data.id === first?.id ? later.data : first;
  const meaning = definition?.definitions?.[0];
  const agent = definition?.agent;
  const agentShown =
    agent?.session !== undefined &&
    (agent.state === "answered" || agentAnswerDue(definition, openedAt));

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

  const tooltips = useTooltips();
  const prose = `text-[13px] leading-5 ${tooltips ? textSecondaryOnSurface : ON_PAGE.prose}`;
  // Inside `NoTooltips` why Dispatch could not define the words, which a `title` holds today, is
  // drawn under what the popover says of it.
  const why = (reason: string | undefined) =>
    tooltips || reason === undefined ? null : (
      <p className={`mt-1 max-w-full text-xs [overflow-wrap:anywhere] ${ON_PAGE.quiet}`}>
        {reason}
      </p>
    );
  return createPortal(
    <div
      aria-label={`What “${words}” means`}
      className={`fixed z-50 rounded-xl border p-3 shadow-lg ${tooltips ? `${card} ${borderDefault}` : ON_PAGE.frame}`}
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
        define.isError ? (
          <>
            <p className={prose} title={tooltips ? define.error.message : undefined}>
              Dispatch could not define these words. Press d to try again.
            </p>
            {why(define.error.message)}
          </>
        ) : (
          <div aria-label="Defining" className="space-y-1.5 py-0.5" role="status">
            <div
              className={`h-3 w-full animate-pulse rounded ${tooltips ? skeletonBg : ON_PAGE.skeleton}`}
            />
            <div
              className={`h-3 w-4/5 animate-pulse rounded ${tooltips ? skeletonBg : ON_PAGE.skeleton}`}
            />
          </div>
        )
      ) : meaning === undefined ? (
        // The record's half gave no meaning (`failure` says why); the agent's may still come.
        <>
          <p className={prose} title={tooltips ? definition.failure : undefined}>
            Dispatch could not define these words.
          </p>
          {why(definition.failure)}
        </>
      ) : (
        <>
          <p className={prose}>{meaning.meaning}</p>
          {(meaning.links ?? []).map((href) => (
            <DefinitionLink href={href} key={href} onFollow={onClose} />
          ))}
        </>
      )}
      {!agentShown || agent?.session === undefined ? null : (
        <div
          className={`mt-2.5 border-t pt-2.5 ${tooltips ? borderDefault : ON_PAGE.agentRule}`}
          data-define-agent={agent.state}
        >
          {/* The agent's name is the one label: it tells its words from the record's. Inside
              `NoTooltips` it is the page's name of the session - An agent for one nothing names -
              and a value, which breaks anywhere beside its dot. */}
          <p
            className={`flex items-center gap-1.5 text-xs font-medium ${tooltips ? textMutedOnSurface : ON_PAGE.quiet}`}
          >
            <span
              aria-hidden="true"
              className={`size-1.5 shrink-0 rounded-full ${tooltips ? liveDotBg : pageSegmentDoneBg}`}
            />
            {tooltips ? (
              agentText.sessionShown(agent.session, undefined)
            ) : (
              <span className="min-w-0 [overflow-wrap:anywhere]">
                {agentText.sessionShown(agent.session, undefined)}
              </span>
            )}
          </p>
          {agent.state === "answered" ? (
            <p className={`mt-1 ${prose}`}>{agent.answer}</p>
          ) : (
            <span aria-label="Asking the agent" className="mt-2 flex gap-1" role="status">
              {[0, 200, 400].map((delay) => (
                <span
                  className={`size-1.5 animate-pulse rounded-full bg-current ${tooltips ? textMutedOnSurface : ON_PAGE.quiet}`}
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
