import {
  actorAttribution,
  actorName,
  sessionLabel,
  shortSessionId,
} from "@legion/contracts/actor-label";
import { type ReactNode, useCallback } from "react";
import { generatePath, Link } from "react-router-dom";

import type { Actor } from "../../api/types";
import { type PageWords, usePageWords, useTooltips } from "../../components/tooltips";
import {
  focusVisibleRing,
  linkHoverText,
  linkText,
  liveDotBg,
  offlineDotBorder,
  pageInkText,
  pageSegmentDoneBg,
  stalledDotBorder,
  unknownDotBorder,
} from "../../theme/shell-classes";
import { type AgentRegistry, type AgentState, useAgentRegistry } from "./agent-registry";
import { AGENT_LIVE_PATH } from "./page-routes";
import { relativeTimeLabel } from "./relative-time";

/**
 * A session as `AgentName` draws it (`AgentName.tsx`): its state mark, its name and its link to
 * the live conversation, and the rule that names it. `AgentName` draws every session through
 * this, so there is one way to show one.
 *
 * WHY A MODULE OF ITS OWN. The rail's pinned agents (`features/sidebar/Sidebar.tsx`) are the one
 * place the entry chunk shows an agent, and the entry is what every page parses before it draws
 * anything (`web/src/asset-sizes-in-build.ts` holds it to its budget). They show a session by its
 * id, and nothing more. Through `AgentName.tsx` the rail would carry all of that module into the
 * entry: a person's name, an author's, the plain-text forms, the resolution lines and the
 * unresponsive note, which only the pages' own chunks draw. So the rail imports this half alone,
 * and `AgentName.tsx` is in the chunks that draw it.
 *
 * With `AgentName.tsx` and `routes.ts`, this is one of the modules the root `biome.json` lets
 * import the contracts naming functions and `AGENT_LIVE_PATH` (`AgentName.tsx` says why).
 */

type SessionActor = Actor & { kind: "session" };

/** Whether an actor is a session's. An actor's kind reaches the browser as the string the route
 *  table declares, and `session` is the one word that names a session; every surface that needs
 *  the session itself narrows here. */
export function isSessionActor(actor: Actor): actor is SessionActor {
  return actor.kind === "session";
}

/** An agent to name: an actor, or a bare session id with the title stamped where it was
 *  recorded. */
export type AgentRef = { actor: Actor } | { sessionId: string; storedTitle: string | undefined };

type SessionRef = { actor: SessionActor } | { sessionId: string; storedTitle: string | undefined };

interface ResolvedAgent {
  /** The session the agent is; `undefined` for a user or system actor. */
  readonly sessionId: string | undefined;
  /** A session's registry title, else the title stamped on the write, else `session:<8>…`; a
   *  user's or system's id. */
  readonly name: string;
  /** What follows the name: ` (for <owner>)`, ` (as <namespace>/<name>)`, or `""`. */
  readonly attribution: string;
}

/** What `resolveAgent` reads of the registry: its titles alone. */
export type RegistryTitles = Pick<AgentRegistry, "title" | "titles">;

/** The name and attribution `main` gives an agent: every name `AgentName.tsx` spells as text
 *  (`AgentText`'s `label`, `part`, `sessionText` and `initials`, on every surface) and every name
 *  it draws outside `NoTooltips` comes from here. Inside `NoTooltips` a drawn session name follows
 *  `drawnSessionName`, and `AgentText.author` names an author, instead. */
export function resolveAgent(
  ref: SessionRef,
  registry: RegistryTitles
): ResolvedAgent & { readonly sessionId: string };
export function resolveAgent(ref: AgentRef, registry: RegistryTitles): ResolvedAgent;
export function resolveAgent(ref: AgentRef, registry: RegistryTitles): ResolvedAgent {
  if ("actor" in ref) {
    const { actor } = ref;
    return {
      attribution: actorAttribution(actor),
      name: actorName(actor, registry.titles),
      sessionId: isSessionActor(actor) ? actor.id : undefined,
    };
  }
  return {
    attribution: "",
    name: sessionLabel(ref.sessionId, registry.title(ref.sessionId) ?? ref.storedTitle),
    sessionId: ref.sessionId,
  };
}

/** A session's name as every form of `AgentName` draws it on the surface it is on, the one rule
 *  for a drawn session name. Everywhere but inside `NoTooltips` (`words` null) it is
 *  `resolveAgent`'s name. Inside, it is the name the new issue page gives an author: the
 *  registry title, else the title stamped on the write, else the page's word for an agent nothing
 *  names (`unnamed`: a word of the page, not a name somebody gave). There it has no
 *  `session:<8>…` fallback: a reader looks to a name for who it is, and a short id names
 *  nobody. */
export function drawnSessionName(
  ref: SessionRef,
  registry: AgentRegistry,
  words: PageWords | null
): { readonly name: string; readonly unnamed: boolean } {
  if (words === null) return { name: resolveAgent(ref, registry).name, unnamed: false };
  const sessionId = "actor" in ref ? ref.actor.id : ref.sessionId;
  const storedTitle = "actor" in ref ? ref.actor.origin?.session_title : ref.storedTitle;
  const title = registry.title(sessionId) ?? storedTitle?.trim();
  return title === undefined || title === ""
    ? { name: words.CONTROL_LABELS.unnamed_author, unnamed: true }
    : { name: title, unnamed: false };
}

/** A bare session id's name as text, the rule `AgentText.sessionText` spells it by
 *  (`useAgentText`, `AgentName.tsx`): its registry title, else `storedTitle`, else its short id,
 *  on every surface. For a part of the entry chunk that names a session in a string - the rail's
 *  Unpin label, the pin write's label (`features/agents/pins.ts`) - and so cannot take
 *  `useAgentText` without the rest of `AgentName.tsx`. Reads the registry titles the cache
 *  already holds and fetches nothing itself. */
export function useSessionText(): (sessionId: string, storedTitle: string | undefined) => string {
  const registry = useAgentRegistry(false);
  return useCallback(
    (sessionId: string, storedTitle: string | undefined) =>
      resolveAgent({ sessionId, storedTitle }, registry).name,
    [registry]
  );
}

export type AgentVariant =
  /** `link` (default): the name links to the live conversation, pushing the address, so Back
   *   walks the pages the reader came from. `replace` is the rail's pinned rows alone
   *   (INFRA-812): a move from one pinned agent to another replaces the live view the reader is
   *   in rather than stacking it. `onNavigate` is told the link was followed, which is how the
   *   rail's sheet closes behind it. `static`: no link, for the places a link cannot sit (a
   *   tooltip, a listbox option, the live page's own header). */
  | {
      kind?: "link" | "static";
      control?: never;
      onNavigate?: () => void;
      replace?: boolean;
    }
  /** `control`: the host keeps its own control around the name, and the live link renders
   *  beside it, outside the host control. */
  | {
      kind: "control";
      control: (name: ReactNode) => ReactNode;
      onNavigate?: never;
      replace?: never;
    };

const LINK_CLASS = `rounded outline-none focus-visible:ring-2 ${focusVisibleRing} ${linkText} ${linkHoverText}`;

/** `LINK_CLASS` inside `NoTooltips`: the new issue page's link, in the ink with its underline,
 *  since ink beside ink says link by the underline alone; the focus ring stays `main`'s, the
 *  application's one focus indicator. */
const PAGE_LINK_CLASS = `rounded outline-none focus-visible:ring-2 ${focusVisibleRing} ${pageInkText} underline`;

const MARK_CLASS = "mr-1 inline-block h-2 w-2 shrink-0 rounded-full align-middle";

/** An agent's name never widens its host (`SessionAgentName` says how). */
export const AGENT_SPAN_CLASS = "min-w-0 [overflow-wrap:anywhere]";

/** Filled, amber-ringed, hollow and dashed: the states read apart without colour. */
const STATE_MARK_CLASS: Record<AgentState, string> = {
  "not-running": `${MARK_CLASS} border-2 border-solid ${offlineDotBorder}`,
  running: `${MARK_CLASS} ${liveDotBg}`,
  unknown: `${MARK_CLASS} border border-dashed ${unknownDotBorder}`,
  unresponsive: `${MARK_CLASS} border-2 border-solid ${stalledDotBorder}`,
};

/** `STATE_MARK_CLASS` inside `NoTooltips`, in the new issue page's colours:
 *  running is the passed green, unresponsive the waiting orange, and not running or unknown the
 *  not-run grey, in the same shapes, which say the state without colour. */
const PAGE_STATE_MARK_CLASS: Record<AgentState, string> = {
  "not-running": `${MARK_CLASS} border-2 border-solid border-page-not-run`,
  running: `${MARK_CLASS} ${pageSegmentDoneBg}`,
  unknown: `${MARK_CLASS} border border-dashed border-page-not-run`,
  unresponsive: `${MARK_CLASS} border-2 border-solid border-page-waiting`,
};

const STATE_LABEL: Record<AgentState, string> = {
  "not-running": "Not running",
  running: "Running",
  unknown: "Running state unknown",
  unresponsive: "Unresponsive",
};

/** What each state's mark says beyond its name. An unresponsive session's whole point is when
 *  it was last heard from - that is what tells a reader whether to wait for it - so its mark
 *  says so as a running one's does, and says that it has not ended. */
const STATE_NOTE: Record<AgentState, string> = {
  "not-running": "",
  running: "",
  unknown: "",
  unresponsive:
    " · it has missed its heartbeats without ending: it keeps its roles and claims, and messages are stored for it",
};

function AgentStateMark({
  lastSeen,
  state,
}: {
  lastSeen: number | undefined;
  state: AgentState;
}): ReactNode {
  const tooltips = useTooltips();
  const seen =
    (state === "running" || state === "unresponsive") && lastSeen !== undefined
      ? relativeTimeLabel(new Date(lastSeen), Date.now())
      : undefined;
  const reading = `${STATE_LABEL[state]}${seen === undefined ? "" : ` · seen ${seen}`}${STATE_NOTE[state]}`;
  // Inside `NoTooltips` (`components/tooltips.tsx`) the whole reading - the state, how long ago
  // a running or unresponsive session was seen, and what an unresponsive one's state means - is
  // the mark's accessible name instead, so nothing is kept for a pointer alone; the shape says
  // the state to everyone either way.
  return (
    <span
      aria-label={tooltips ? STATE_LABEL[state] : reading}
      className={(tooltips ? STATE_MARK_CLASS : PAGE_STATE_MARK_CLASS)[state]}
      role="img"
      title={tooltips ? reading : undefined}
    />
  );
}

/**
 * A session, as `AgentName` draws one: in one `<span data-agent-name data-agent-state>`, its
 * state mark, its name, and its attribution. The rail's pinned rows draw it themselves, by the
 * session's id, as `<AgentName sessionId>` would (this module says why).
 */
export function SessionAgentName({
  className,
  inline,
  subject,
  variant,
}: {
  className?: string;
  inline?: boolean;
  subject: SessionRef;
  variant: AgentVariant;
}): ReactNode {
  const tooltips = useTooltips();
  const page = usePageWords();
  const registry = useAgentRegistry(true);
  const { attribution, sessionId } = resolveAgent(subject, registry);
  // Every form names a session by the one rule for a drawn session name, whether the host gave
  // the session's actor or its bare id: inside `NoTooltips` by a title, else the page's word, and
  // never by its short id, an address that names nobody. `data-agent-name` keeps the full id.
  const { name, unnamed } = drawnSessionName(subject, registry, page);
  // The page's word for an agent nothing names is the page's own, so inside `NoTooltips` it
  // carries that mark and stands outside the mark of a name somebody was given.
  const fallbackMark = unnamed ? page?.pageChrome : undefined;
  const shownName = fallbackMark === undefined ? name : <span {...fallbackMark}>{name}</span>;
  const state = registry.state(sessionId);
  const linkClass = tooltips ? LINK_CLASS : PAGE_LINK_CLASS;
  const to = generatePath(AGENT_LIVE_PATH, { sessionId: encodeURIComponent(sessionId) });

  let named: ReactNode;
  let liveLink: ReactNode = null;
  if (variant.kind === "control") {
    named = variant.control(shownName);
    const openLabel = `Open live conversation with ${name}${attribution}`;
    liveLink = (
      <Link
        aria-label={openLabel}
        className={`ml-1 px-1 ${linkClass}`}
        title={tooltips ? openLabel : undefined}
        to={to}
      >
        <span aria-hidden="true">↗</span>
      </Link>
    );
  } else if (variant.kind === "static") {
    named = shownName;
  } else {
    // No `aria-label`: the link is named by its visible name, and the attribution after it is
    // read once, as text. Inside a sentence it is underlined, as every other inline link of the
    // app is: the link blue alone does not reach 3:1 against the text around it in dark, so
    // colour on its own would be the only thing marking it a link (axe's `link-in-text-block`).
    named = (
      <Link
        className={inline ? `${linkClass} dispatch-inline-link underline` : linkClass}
        onClick={variant.onNavigate}
        replace={variant.replace === true}
        to={to}
      >
        {shownName}
      </Link>
    );
  }

  // The name never widens its host: a flex item may shrink below its text (`min-w-0`), and a
  // long title with no space breaks mid-word; a host's `truncate` (nowrap) still ellipsizes.
  // The short form `session:<8>…` does not tell sessions apart (ids minted in the same minute
  // share it), so an agent named by it carries its full id as its tooltip. Inside `NoTooltips`
  // the name, and the person it acted for, are the names they were given, not words the new
  // issue page declares, so the span carries that mark; where the page's word stands for an
  // agent nothing names, only what follows it carries the mark.
  return (
    <span
      className={className === undefined ? AGENT_SPAN_CLASS : `${AGENT_SPAN_CLASS} ${className}`}
      data-agent-name={sessionId}
      data-agent-state={state}
      title={tooltips && name === shortSessionId(sessionId) ? sessionId : undefined}
      {...(fallbackMark === undefined ? page?.authorText : undefined)}
    >
      <AgentStateMark lastSeen={registry.lastSeen(sessionId)} state={state} />
      {named}
      {fallbackMark === undefined || attribution === "" ? (
        attribution
      ) : (
        <span {...page?.authorText}>{attribution}</span>
      )}
      {liveLink}
    </span>
  );
}
