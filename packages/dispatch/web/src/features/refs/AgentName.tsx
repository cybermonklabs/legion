import {
  actorActText,
  actorAttribution,
  actorName,
  sessionLabel,
  shortSessionId,
} from "@legion/contracts";
import { Fragment, type ReactNode, useMemo } from "react";
import { generatePath, Link } from "react-router-dom";

import type { Actor, AskResolution } from "../../api/types";
import { type PageWords, usePageWords, useTooltips } from "../../components/tooltips";
import {
  focusVisibleRing,
  inlineWarningText,
  linkHoverText,
  linkText,
  liveDotBg,
  offlineDotBorder,
  pageInkText,
  pageSegmentDoneBg,
  pageStateText,
  stalledDotBorder,
  unknownDotBorder,
} from "../../theme/classes";
import { askResolutionBadge } from "./actor";
import { type AgentRegistry, type AgentState, useAgentRegistry } from "./agent-registry";
import { AGENT_LIVE_PATH } from "./routes";
import { relativeTimeLabel, Timestamp } from "./Timestamp";

/**
 * How Dispatch shows an agent (a session), in one place: named by its current registry title
 * when the registry lists it, else by today's rule (the title stamped on the write, else
 * `session:<8>…`; inside `NoTooltips`, the new issue page's word for an agent nothing names),
 * with its running state from the registry and a link to its live conversation. This module
 * owns the SPA's use of the contracts naming functions; the reference short form in `routes.ts`
 * is the one other reader, and it does not show an agent. The root `biome.json` bans importing
 * them anywhere else in `web/src` (and `AGENT_LIVE_PATH` outside `app.tsx`, which mounts the
 * route), but not through a namespace import (`import * as contracts from "@legion/contracts"`):
 * lint misses that member use, so a review looks for it.
 */

type SessionActor = Actor & { kind: "session" };

/** Whether an actor is a session's. An actor's kind reaches the browser as the string the route
 *  table declares, and `session` is the one word that names a session; every surface that needs
 *  the session itself narrows here. */
function isSessionActor(actor: Actor): actor is SessionActor {
  return actor.kind === "session";
}

/** An agent to name: an actor, or a bare session id with the title stamped where it was
 *  recorded. */
type AgentRef = { actor: Actor } | { sessionId: string; storedTitle: string | undefined };

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
type RegistryTitles = Pick<AgentRegistry, "title" | "titles">;

/** The name and attribution `main` gives an agent: every name this module spells as text
 *  (`AgentText`'s `label`, `part`, `sessionText` and `initials`, on every surface) and every name
 *  it draws outside `NoTooltips` comes from here. Inside `NoTooltips` a drawn session name follows
 *  `drawnSessionName`, and `AgentText.author` names an author, instead. */
function resolveAgent(
  ref: SessionRef,
  registry: RegistryTitles
): ResolvedAgent & { readonly sessionId: string };
function resolveAgent(ref: AgentRef, registry: RegistryTitles): ResolvedAgent;
function resolveAgent(ref: AgentRef, registry: RegistryTitles): ResolvedAgent {
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

/** No registry title at all: what an agent is named by before the registry answers, or where
 *  no registry is read (`actorLabelText`). */
const NO_TITLES: RegistryTitles = { title: () => undefined, titles: new Map() };

/** `AgentText.label`'s words for an agent, by the titles `registry` holds: its name, with the
 *  agent a person's act was made through, then its attribution. */
function labelText(ref: AgentRef, registry: RegistryTitles): string {
  const { attribution, name } = resolveAgent(ref, registry);
  // The same name `AgentName` draws, the agent an act was made through included, so a title
  // or an aria-label never says a person wrote what their agent wrote for them.
  const acted =
    "actor" in ref && ref.actor.kind === "user" ? actorActText(name, ref.actor.act) : name;
  return `${acted}${attribution}`;
}

type AgentSubject =
  | { actor: Actor; sessionId?: never; storedTitle?: never }
  | { actor?: never; sessionId: string; storedTitle: string | undefined };

type AgentVariant =
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

type AgentNameProps = AgentSubject &
  AgentVariant & {
    /** Inside `NoTooltips`, names a person as the new issue page names an author
     *  (`AgentText.author`): an act made through a session names the agent by what it is.
     *  A session is named as an author is in every form there, with or without `author`.
     *  Everywhere else it changes nothing. */
    author?: boolean;
    className?: string;
    /** Inside a sentence: the link keeps its line box (`.dispatch-inline-link`, `styles.css`). */
    inline?: boolean;
    /** The words the session's state mark says, where its host words them (`AgentStateWords`):
     *  the issue page's box, on its target line and in its picker's agent rows
     *  (`RefPickerWords.marks`). With none, the mark's own (`STATE_WORDS`). */
    stateWords?: AgentStateWords;
  };

const LINK_CLASS = `rounded outline-none focus-visible:ring-2 ${focusVisibleRing} ${linkText} ${linkHoverText}`;

/** `LINK_CLASS` inside `NoTooltips`: the new issue page's link, in the ink with its underline,
 *  since ink beside ink says link by the underline alone; the focus ring stays `main`'s, the
 *  application's one focus indicator. */
const PAGE_LINK_CLASS = `rounded outline-none focus-visible:ring-2 ${focusVisibleRing} ${pageInkText} underline`;

const MARK_CLASS = "mr-1 inline-block h-2 w-2 shrink-0 rounded-full align-middle";

const AGENT_SPAN_CLASS = "min-w-0 [overflow-wrap:anywhere]";

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

/** What a session's state mark says: the name of each state (`labels`), what stands between it
 *  and how long ago a running or unresponsive session was seen (`seen`), and what an unresponsive
 *  one's state means, after both (`unresponsive`). A host that words the mark gives its own
 *  (`AgentName`'s `stateWords`). */
export interface AgentStateWords {
  readonly labels: Readonly<Record<AgentState, string>>;
  readonly seen: string;
  readonly unresponsive: string;
}

/** The state mark's own words, on every surface whose host gives none. */
const STATE_WORDS: AgentStateWords = {
  labels: STATE_LABEL,
  seen: " · seen ",
  unresponsive: STATE_NOTE.unresponsive,
};

function AgentStateMark({
  lastSeen,
  state,
  words = STATE_WORDS,
}: {
  lastSeen: number | undefined;
  state: AgentState;
  words?: AgentStateWords;
}): ReactNode {
  const tooltips = useTooltips();
  const seen =
    (state === "running" || state === "unresponsive") && lastSeen !== undefined
      ? relativeTimeLabel(new Date(lastSeen), Date.now())
      : undefined;
  const reading = `${words.labels[state]}${seen === undefined ? "" : `${words.seen}${seen}`}${state === "unresponsive" ? words.unresponsive : ""}`;
  // Inside `NoTooltips` (`components/tooltips.tsx`) the whole reading - the state, how long ago
  // a running or unresponsive session was seen, and what an unresponsive one's state means - is
  // the mark's accessible name instead, so nothing is kept for a pointer alone; the shape says
  // the state to everyone either way.
  return (
    <span
      aria-label={tooltips ? words.labels[state] : reading}
      className={(tooltips ? STATE_MARK_CLASS : PAGE_STATE_MARK_CLASS)[state]}
      role="img"
      title={tooltips ? reading : undefined}
    />
  );
}

/**
 * The visible mark beside an unresponsive session on a surface that lists sessions (the Agents
 * page's row, its live conversation's header). The state mark alone is a ring a reader must
 * hover to read, and this state is the one a person has to act on - wait for the session, or
 * decide it is lost - so it is also said in words, with the heartbeat it was last seen at.
 * Renders nothing for any other state, so a page of running sessions stays quiet.
 */
export function AgentUnresponsiveNote({
  className,
  lastSeen,
  state,
}: {
  className?: string;
  lastSeen: number | undefined;
  state: AgentState;
}): ReactNode {
  const tooltips = useTooltips();
  if (state !== "unresponsive") {
    return null;
  }
  const seen = lastSeen === undefined ? undefined : new Date(lastSeen).toISOString();
  return (
    <span
      // Inside `NoTooltips` the words are the new issue page's warning, its waiting orange,
      // and the note narrows to its line rather than keep its width, since it holds the
      // heartbeat's instant, a value that wraps there.
      className={`${className === undefined ? "" : `${className} `}${tooltips ? "shrink-0" : "min-w-0 max-w-full"} text-xs ${tooltips ? inlineWarningText : pageStateText.waiting}`}
      data-testid="agent-unresponsive"
      title={
        tooltips
          ? `This session has missed its heartbeats${seen === undefined ? "" : ` since ${new Date(seen).toLocaleString()}`}. It has not ended: it keeps its roles and claims, and messages sent to it are stored until it answers.`
          : undefined
      }
    >
      Unresponsive
      {seen === undefined ? null : (
        <>
          {" "}
          · seen <Timestamp at={seen} />
        </>
      )}
    </span>
  );
}

/**
 * An agent, as every place in the app shows one. A `user` or `system` actor is its id as plain
 * text (inside the host's control, for `kind="control"`) and reads no registry; a person an
 * agent acted for also carries that agent (`actorActText`). A session renders in one
 * `<span data-agent-name data-agent-state>`: its state mark, its name, and its attribution.
 */
export function AgentName(props: AgentNameProps): ReactNode {
  const page = usePageWords();
  if (props.sessionId !== undefined) {
    return (
      <SessionAgentName
        className={props.className}
        inline={props.inline}
        stateWords={props.stateWords}
        subject={{ sessionId: props.sessionId, storedTitle: props.storedTitle }}
        variant={props}
      />
    );
  }
  const { actor } = props;
  if (!isSessionActor(actor)) {
    // An act an agent made for its person is recorded as the person's, so the person's name
    // carries the agent beside it, by the one rule every reader of an act follows.
    const text = actor.kind === "user" ? actorActText(actor.id, actor.act) : actor.id;
    const name = page === null ? text : markedPersonName(actor, props.author === true, text, page);
    const named = props.kind === "control" ? props.control(name) : name;
    return props.className === undefined ? named : <span className={props.className}>{named}</span>;
  }
  return (
    <SessionAgentName
      className={props.className}
      inline={props.inline}
      stateWords={props.stateWords}
      subject={{ actor }}
      variant={props}
    />
  );
}

function SessionAgentName({
  className,
  inline,
  stateWords,
  subject,
  variant,
}: {
  className: string | undefined;
  inline: boolean | undefined;
  stateWords?: AgentStateWords;
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
      <AgentStateMark lastSeen={registry.lastSeen(sessionId)} state={state} words={stateWords} />
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

/** A Conversation avatar's initials: the first letter of the first two words of an agent's
 *  name (never its attribution). */
function agentInitials(name: string): string {
  return name
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((part) => part[0]?.toUpperCase())
    .join("");
}

export interface AgentText {
  /** Who wrote a thing, as the new issue page names an author.
   *  Inside `NoTooltips`: a session as `sessionShown` names it there (by its registry title, else
   *  the title stamped on its write, else the page's word for an agent nothing names, and never by
   *  its short id); a person by their login, with an act made through a session named by what it
   *  is; and the page's words for no actor. Everywhere else: an actor as `label` names it, and no
   *  actor as "". */
  author(actor: Actor | undefined): string;
  /** The full label: name plus ` (for <owner>)` / ` (as ns/name)`. The name is `main`'s on every
   *  surface (`resolveAgent`): inside `NoTooltips` too, a session nothing names is its short id,
   *  not the page's word that `AgentName` draws there. */
  label(actor: Actor): string;
  /** An `AgentPart` as text: a text part as is, an agent part as `label` spells it, which is the
   *  rule `AgentParts` draws it by everywhere but inside `NoTooltips`. */
  part(part: AgentPart): string;
  /** A bare session id's name: its registry title, else `storedTitle`, else its short id, on every
   *  surface. The mention composer stores it as a mention's `@` text, where the short id still
   *  names the session and the page's word for an agent nothing names would name nobody. */
  sessionText(sessionId: string, storedTitle: string | undefined): string;
  /** A bare session id's name as `AgentName` draws it on this surface (`drawnSessionName`), for a
   *  sentence a reader takes beside the drawn name, such as a dialog's question or a screen
   *  reader's words: everywhere but inside `NoTooltips` it is `sessionText`; inside, its registry
   *  title, else `storedTitle`, else the page's word for an agent nothing names, and never its
   *  short id. Never for text that is stored or sent: that is `sessionText`. */
  sessionShown(sessionId: string, storedTitle: string | undefined): string;
  /** The Conversation avatar's initials (`agentInitials`). */
  initials(actor: Actor): string;
}

/**
 * `AgentText.label` for a caller that reads no agent registry, such as a pure model: the label
 * `useAgentText` spells before the registry has answered, or with no registry. A person by login,
 * with the agent an act was made through; a session by the title stamped on its write, else its
 * short id (`session:<8>…`); and the attribution after either. Where the registry lists a session
 * under a title, `label` names it by that title and this does not.
 */
export function actorLabelText(actor: Actor): string {
  return labelText({ actor }, NO_TITLES);
}

/** A session's name as every form of `AgentName` draws it on the surface it is on, the one rule
 *  for a drawn session name. Everywhere but inside `NoTooltips` (`words` null) it is
 *  `resolveAgent`'s name. Inside, it is the name the new issue page gives an author: the
 *  registry title, else the title stamped on the write, else the page's word for an agent nothing
 *  names (`unnamed`: a word of the page, not a name somebody gave). There it has no
 *  `session:<8>…` fallback: a reader looks to a name for who it is, and a short id names
 *  nobody. */
function drawnSessionName(
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

/** `AgentText.author` inside `NoTooltips`: a session as `drawnSessionName` names it there, a
 *  person as `personAuthor` does, and no actor by the page's words for an author nobody
 *  recorded. */
function authorName(actor: Actor | undefined, registry: AgentRegistry, words: PageWords): string {
  if (actor === undefined) return words.CONTROL_LABELS.unknown_author;
  if (!isSessionActor(actor)) {
    const { login, through } = personAuthor(actor, words);
    return `${login}${through}`;
  }
  return drawnSessionName({ actor }, registry, words).name;
}

/** A person, or Dispatch itself, as an author: by login, with the agent an act was made through
 *  named by what it is rather than by its session's id (`actorActText` names the id). `through`
 *  is what follows the login for an act made through a session, in the page's own words, and
 *  empty for any other act. */
function personAuthor(
  actor: Actor,
  words: PageWords
): { readonly login: string; readonly through: string } {
  const session = actor.act?.session;
  return session === undefined || session === ""
    ? { login: actorActText(actor.id, actor.act), through: "" }
    : { login: actor.id, through: `, acting through ${words.AUTHOR_KIND_WORDS.agent}` };
}

/** A person's or Dispatch's name inside `NoTooltips`, with its marks:
 *  the login is a name somebody was given, and what the page says after an author's login of
 *  an act made through a session is the page's own. `text` is the name as `main` draws it. The
 *  login is a value, so it breaks anywhere and never widens its host, as a session's name does
 *  (`AGENT_SPAN_CLASS`). */
function markedPersonName(
  actor: Actor,
  author: boolean,
  text: string,
  words: PageWords
): ReactNode {
  if (!author) {
    return (
      <span className={AGENT_SPAN_CLASS} {...words.authorText}>
        {text}
      </span>
    );
  }
  const { login, through } = personAuthor(actor, words);
  return (
    <>
      <span className={AGENT_SPAN_CLASS} {...words.authorText}>
        {login}
      </span>
      {through === "" ? null : <span {...words.pageChrome}>{through}</span>}
    </>
  );
}

/**
 * An agent's name as plain text, for the places that can hold only a string: an `aria-label` or
 * `title`, `document.title`, a confirmation dialog's sentence, the composer's `@Title` mention
 * text and its notices, a clipboard value, the Inbox empty state. Never a visible name in a
 * byline, chip, heading or sentence: that is `AgentName`. `label`, `part` and `sessionText` spell
 * a name by the rule `AgentName` draws by outside `NoTooltips`, on every surface, so text that is
 * stored or sent names the same session wherever it was written; `sessionShown` and `author` say
 * what `AgentName` draws on the surface they are read on. Reads the registry titles the cache
 * already holds and fetches nothing itself.
 */
export function useAgentText(): AgentText {
  const registry = useAgentRegistry(false);
  const page = usePageWords();
  return useMemo(() => {
    const label = (ref: AgentRef) => labelText(ref, registry);
    return {
      author: (actor: Actor | undefined) =>
        page !== null
          ? authorName(actor, registry, page)
          : actor === undefined
            ? ""
            : label({ actor }),
      initials: (actor: Actor) => agentInitials(resolveAgent({ actor }, registry).name),
      label: (actor: Actor) => label({ actor }),
      part: (part: AgentPart) => (typeof part === "string" ? part : label(part)),
      sessionShown: (sessionId: string, storedTitle: string | undefined) =>
        drawnSessionName({ sessionId, storedTitle }, registry, page).name,
      sessionText: (sessionId: string, storedTitle: string | undefined) =>
        resolveAgent({ sessionId, storedTitle }, registry).name,
    };
  }, [page, registry]);
}

/** The text before and after a resolution's actor. A withdrawal is the close of the ask's issue,
 *  so it names the closer: `Withdrawn when <agent> closed the issue`. A decline is the person's
 *  own act, so it says what they did: `<agent> declined to answer`. Else `<Verb> by <agent>`. */
function resolutionFrame(resolution: AskResolution): readonly [before: string, after: string] {
  if (resolution.kind === "withdrawn") return ["Withdrawn when ", " closed the issue"];
  if (resolution.kind === "declined") return ["", " declined to answer"];
  return [`${askResolutionBadge(resolution)} by `, ""];
}

/** `Resolved by <agent>` / `Retracted by <agent>` / `Withdrawn when <agent> closed the issue` /
 *  `<agent> declined to answer`: the actor sentence of a resolution, for callers that render its
 *  reason (`askResolutionReason`) with their own markup. `agent="static"` names the agent
 *  without its link, for a host that is itself one control (a listbox option). */
export function AskResolutionLine({
  agent,
  resolution,
}: {
  agent?: "link" | "static";
  resolution: AskResolution;
}): ReactNode {
  const [before, after] = resolutionFrame(resolution);
  return (
    <>
      {before}
      <AgentName actor={resolution.actor} inline kind={agent} />
      {after}
    </>
  );
}

/**
 * A sentence that names agents, kept as parts until it is rendered: text, or an agent that
 * `AgentParts` draws through `AgentName`. A builder that describes an event returns these, so
 * the agent keeps its identity (its current name, state and live link) instead of being
 * flattened into a label. A role address is text; only a recorded session is an agent part.
 */
export type AgentPart = string | AgentRef;

/** Draws `AgentPart`s in a line of text: each text part as is (or through `text`, which the
 *  host uses to mark up its own text), each agent part as an inline `AgentName`. */
export function AgentParts({
  parts,
  text,
}: {
  parts: readonly AgentPart[];
  text?: (part: string, index: number) => ReactNode;
}): ReactNode {
  return parts.map((part, index) => (
    // biome-ignore lint/suspicious/noArrayIndexKey: the parts are one fixed sentence, in order.
    <Fragment key={index}>
      {typeof part === "string" ? (
        text === undefined ? (
          part
        ) : (
          text(part, index)
        )
      ) : (
        <AgentName {...part} inline />
      )}
    </Fragment>
  ));
}

/** `Resolved by <agent>` / `Retracted by <agent>` / `Withdrawn when <agent> closed the issue` /
 *  `<agent> declined to answer` as `AgentPart`s, for the description builders (the activity and
 *  event descriptions). The reason is no part of it: it is the resolver's own stored text, which
 *  a builder adds as a part the one reader draws (`askResolutionReason`, `SentenceParts`). */
export function askResolutionParts(resolution: AskResolution): AgentPart[] {
  const [before, after] = resolutionFrame(resolution);
  return [before, { actor: resolution.actor }, after];
}
