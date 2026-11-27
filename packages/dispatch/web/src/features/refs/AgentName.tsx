import { actorActText } from "@legion/contracts/actor-label";
import { Fragment, type ReactNode, useMemo } from "react";

import type { Actor, AskResolution } from "../../api/types";
import { type PageWords, usePageWords, useTooltips } from "../../components/tooltips";
import { inlineWarningText, pageStateText } from "../../theme/classes";
import { askResolutionBadge } from "./actor";
import { type AgentRegistry, type AgentState, useAgentRegistry } from "./agent-registry";
import {
  AGENT_SPAN_CLASS,
  type AgentRef,
  type AgentStateWords,
  type AgentVariant,
  drawnSessionName,
  isSessionActor,
  type RegistryTitles,
  resolveAgent,
  SessionAgentName,
} from "./SessionName";
import { Timestamp } from "./Timestamp";

/**
 * How Dispatch shows an agent (a session), in one place: named by its current registry title
 * when the registry lists it, else by today's rule (the title stamped on the write, else
 * `session:<8>…`; inside `NoTooltips`, the new issue page's word for an agent nothing names),
 * with its running state from the registry and a link to its live conversation. A session is
 * drawn, and named, by this module's session half (`SessionName.tsx`, which says why it is a
 * module of its own). The two own the SPA's use of the contracts naming functions; the
 * reference short form in `routes.ts` is the one other reader, and it does not show an agent.
 * The root `biome.json` bans importing them anywhere else in `web/src` (and `AGENT_LIVE_PATH`
 * outside `app.tsx`, which mounts the route), but not through a namespace import
 * (`import * as contracts from "@legion/contracts"`):
 * lint misses that member use, so a review looks for it. This search from the repository root
 * lists each use whatever it was imported from, and the review reads every hit that comes
 * through a namespace member:
 *
 *     rg -n --glob '!*.test.*' 'liveDotBg|offlineDotBorder|stalledDotBorder|unknownDotBorder|/live`|AGENT_LIVE_PATH|actorAttribution|actorLabel|actorName|sessionLabel|shortSessionId|session_title' packages/dispatch/web/src
 */

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
     *  (`RefPickerWords.marks`). With none, the mark's own (`SessionName.tsx`'s `STATE_WORDS`). */
    stateWords?: AgentStateWords;
  };

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
