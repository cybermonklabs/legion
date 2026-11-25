/**
 * The grammar of `dispatch://` references and the addresses that no module of the entry chunk
 * reads: a reference's route, its dashboard path and its short form, the pages of an agent's
 * artifact and of a machine, and the words of the Inbox's filters in its address. The page
 * addresses that the shell reads are in `page-routes.ts`, which this module imports: the entry
 * chunk imports that module and not this one, so this module and the modules that only it
 * imports stay out of the entry.
 */
import { shortSessionId } from "@legion/contracts/actor-label";
import { decodeBlockId } from "@legion/contracts/dispatch-block-ref";
import { hasControlCharacter, itemFromSearch } from "@legion/contracts/dispatch-href";
import { isSessionId, SESSION_ID_PATTERN } from "@legion/contracts/dispatch-text-rules";
import { ASK_URGENCIES } from "@legion/contracts/dispatch-tools-constants";

import type { ArtifactOwner } from "../../api/client";
import type { Artifact } from "../../api/types";
import { INBOX_ROW_TYPES } from "../inbox/list-row";
import { EVERYONE_FILTER_VALUE } from "../inbox/sections";
import { blockFragment, itemOfBlock, parseBlockReference, withBlock } from "./block-reference";
import {
  artifactRoute,
  buildIssuePath,
  buildProjectPath,
  type DispatchRoute,
  decodedSegment,
  decodedSlugAndVersion,
  INBOX_FILTER_FIELDS,
  type InboxFilterField,
  type InboxRoute,
  type IssueRoute,
  isPlainIssueRoute,
  issueKeyPattern,
  type ProjectDocumentRoute,
  type ProjectRoute,
  parseIssuePath,
  parseProjectPath,
  plainIssueTab,
  projectDocumentRoute,
  projectKeyPattern,
} from "./page-routes";
import { pageInBound, routeInBound } from "./target-id";

/** An artifact of an agent's conversation - a picture sent in a direct message on the Agents
 *  page - named by the agent's session id as written, at `/agents/<session id>/artifacts/<slug>`. */
export type AgentArtifactRoute = {
  kind: "agent-artifact";
  session: string;
  slug: string;
  version?: number;
};
/** A component of a project's architecture, `dispatch://<PROJECT>/component/<id>`, shown on the
 *  project's Architecture tab. */
export type ComponentRoute = { kind: "component"; project: string; id: string };
/**
 * A party a reference names: the agent whose session id it carries, the role a token names, or
 * the person a login names. A party is a reader rather than a record, so the reference graph
 * stores no node and no edge for one (Go's `refs.TargetOf`), and a person has no page at all
 * (`buildReferencePath`).
 */
export type PartyRoute =
  | { kind: "agent-session"; session: string }
  | { kind: "role"; role: string }
  | { kind: "person"; login: string };
export type DispatchReferenceRoute =
  | IssueRoute
  | ProjectDocumentRoute
  | AgentArtifactRoute
  | ComponentRoute
  | PartyRoute;
/** A reference that opens a page of this dashboard: every kind but a person, whom nothing
 *  shows (R5). It is what `buildReferencePath` takes, so a sink holding a reference a person
 *  typed has to answer for that kind itself - drawing its words where a link would be - rather
 *  than writing a path that names no page. */
export type PagedReferenceRoute = Exclude<DispatchReferenceRoute, { kind: "person" }>;
/**
 * The five fields the Inbox filters, sorts and groups by. Assignee is one of them, but it keeps
 * a vocabulary of its own in the address - repeated `?assignee=`, with `everyone` and
 * `unassigned` - because a person's email is not a word this page invented, so it rides in
 * `InboxRoute.assignees` rather than in `InboxRoute.filters` (`page-routes.ts`).
 */
export const INBOX_DIMENSIONS = ["priority", "urgency", "type", "project", "assignee"] as const;
export type InboxDimension = (typeof INBOX_DIMENSIONS)[number];

/** How the list is ordered, `?sort=`; `newest` is the default and is left out of the address. */
export const INBOX_SORTS = ["newest", ...INBOX_DIMENSIONS] as const;
export type InboxSort = (typeof INBOX_SORTS)[number];

/** How the list is banded, `?group=`; `priority` is the default and is left out of the address.
 *  `waiting-on` is the turn banding the Inbox had before it had any other, the one
 *  `?section=needs-you` narrows to: the address keeps that older name for the link that was
 *  already written, and Group by says `waiting-on` for the same idea. */
export const INBOX_GROUP_BYS = [...INBOX_DIMENSIONS, "waiting-on", "none"] as const;
export type InboxGroupBy = (typeof INBOX_GROUP_BYS)[number];

/** Which of the reader's two lists is shown, the View menu's **Show**: their Inbox - every
 *  undismissed row, read or not - or what they have dismissed out of it. `inbox` is the default
 *  and is left out of the address, where the other is `?show=dismissed`. */
export const INBOX_SHOWS = ["inbox", "dismissed"] as const;
export type InboxShow = (typeof INBOX_SHOWS)[number];

/** The value every filter uses for a row the field says nothing about: an issue with no
 *  priority, a thread with no urgency, a row belonging to no project. */
export const NO_FILTER_VALUE = "none";

/** The values each filter field offers, in the order the control lists them. A project key is
 *  whatever the workspace holds, so that field takes any non-empty word and `none`. */
export const INBOX_PRIORITY_VALUES = ["P0", "P1", "P2", "P3", NO_FILTER_VALUE] as const;
export const INBOX_URGENCY_VALUES = [...ASK_URGENCIES, NO_FILTER_VALUE] as const;
export const INBOX_TYPE_VALUES = INBOX_ROW_TYPES;

/** The slug of an issue's `decisions.md`, the document its Decisions tab is a view of. */
export const DECISIONS_DOCUMENT_SLUG = "decisions-md";

const issueKeyOnlyPattern = new RegExp(`^${issueKeyPattern}$`);
const projectKeyOnlyPattern = new RegExp(`^${projectKeyPattern}$`);
/** The slug Dispatch gives a document, the one a reference can name. */
const artifactSlugPattern = "[a-z0-9]+(?:-[a-z0-9]+)*";
const artifactSlugOnlyPattern = new RegExp(`^${artifactSlugPattern}$`);
const issueArtifactReferencePattern = new RegExp(
  `^artifact/(${artifactSlugPattern})(?:@v([1-9]\\d*))?$`
);
const projectArtifactReferencePattern = new RegExp(
  `^artifact/(${artifactSlugPattern})(?:@v([1-9]\\d*))?(?:/(ask|comment)/([^/?#\\s]+))?$`
);
/** An agent artifact's reference after `dispatch://agent/`: the session id by the server's rule
 *  (`SESSION_ID_PATTERN`), then the artifact's slug and the version it pins. */
const agentArtifactReferencePattern = new RegExp(
  `^(${SESSION_ID_PATTERN})/artifact/(${artifactSlugPattern})(?:@v([1-9]\\d*))?$`,
  "u"
);
const agentArtifactPathPattern = /^\/agents\/([^/]+)\/artifacts\/([^/]+)$/;
/** A component's reference after `dispatch://<PROJECT>/`: the component id, the lowercase slug
 *  the server reads (`text.ComponentIDPattern`). */
const componentReferencePattern = /^component\/([a-z0-9](?:[a-z0-9-]*[a-z0-9])?)$/;
/** A role's token, the charset a `role:<token>` delivery route is written in (the server's
 *  `model.ParseRoute`). */
const roleTokenOnlyPattern = /^[a-z0-9-]+$/;
/** A session id a `session:<id>` delivery route is written with, the server's `model.ParseRoute`
 *  again. It is narrower than the ids a reference may name (`SESSION_ID_PATTERN`, which takes
 *  `.` and `:` among the rest), so a body can write an agent address no route reaches. */
const sessionRouteIDPattern = /^[A-Za-z0-9_-]+$/;
/** A person's login: their email lowercased, the stored form of a login (the server's
 *  `model.CanonicalLogin`). A login written any other way names nobody Dispatch holds. */
const personLoginOnlyPattern = /^[a-z0-9._%+'-]+@[a-z0-9-]+(\.[a-z0-9-]+)+$/;
/** One machine's page: `/machines/<machine>`, the machine name as one percent-encoded segment. */
const machinePathPattern = /^\/machines\/([^/]+)$/;

/** Agents reference the conversation as `dispatch://KEY/log`; the browser path is `/conversation`. */
const legacyLogReferencePattern = /^log$/;

export function isProjectRoute(
  route: DispatchRoute | DispatchReferenceRoute
): route is ProjectRoute {
  return (
    route.kind === "project" ||
    route.kind === "architecture" ||
    route.kind === "issues" ||
    route.kind === "documents" ||
    route.kind === "document"
  );
}

/** An agent's artifact route; the session id is taken as written, the slug and version as an
 *  issue artifact's are. */
function agentArtifactRoute(
  session: string,
  slug: string,
  versionValue: string | null | undefined
): AgentArtifactRoute | undefined {
  const parsed = decodedSlugAndVersion(slug, versionValue);
  if (!isSessionId(session) || parsed === undefined) {
    return undefined;
  }
  return parsed.version === undefined
    ? { kind: "agent-artifact", session, slug: parsed.slug }
    : { kind: "agent-artifact", session, slug: parsed.slug, version: parsed.version };
}

/** The route of an issue's `dispatch://` reference by what follows its key. Nothing, or nothing
 *  after a trailing slash (`dispatch://CORE-1/`), is the issue itself, as the server's
 *  `text.parseDispatch` reads it; every other empty tail - `artifact/`, `ask/`, `spec/`, a second
 *  slash - names nothing there, and nothing here. */
function parseIssueReference(key: string, target: string | undefined): IssueRoute | undefined {
  if (target === undefined || target === "") {
    return { key, kind: "issue" };
  }
  if (target === "spec") {
    return { key, kind: "spec" };
  }
  const plain = plainIssueTab(target);
  if (plain !== undefined) {
    return { key, kind: plain };
  }
  if (legacyLogReferencePattern.test(target) || target === "conversation") {
    return { key, kind: "conversation" };
  }
  const artifact = target.match(issueArtifactReferencePattern);
  if (artifact !== null) {
    return artifactRoute(key, artifact[1] ?? "", artifact[2]);
  }
  const item = target.match(/^(ask|comment|message)\/([^/?#\s]+)$/);
  const id = item?.[2] === undefined ? undefined : decodedSegment(item[2]);
  if (item === null || id === undefined) {
    return undefined;
  }
  return {
    id,
    key,
    kind: item[1] === "ask" ? "ask" : item[1] === "comment" ? "comment" : "message",
  };
}

/** A `dispatch://` reference's route as text cites it, which the server's `text.Extract` indexes:
 * one holding a control character names nothing, and so does one past the bounds the graph keys
 * (`routeInBound`); a fragment names a block (`parseBlockReference`, `withBlock`) or nothing. Only
 * where plain text becomes a reference - linkifying or shortening a bare `dispatch://` string,
 * titling a URL an author left as text in a rendered body (`referenceRouteFromHref`), reading a
 * composer's text - since that must match what the server indexes. */
export function parseDispatchReference(value: string): DispatchReferenceRoute | undefined {
  const route = parseNodeReference(value);
  return route !== undefined && routeInBound(route) ? route : undefined;
}

/** A node's own `dispatch://` reference - one the API returned for a node, one the dashboard wrote
 * for one it shows, or one an author named explicitly as a link's target or a picture's source -
 * as its route, whatever the length of its target id or its block's id: the bounds
 * `parseDispatchReference` holds text to decide only what text cites, and drawing or opening a
 * node is a read, which the server answers for a node of any name by its own reference. */
export function parseNodeReference(value: string): DispatchReferenceRoute | undefined {
  if (hasControlCharacter(value)) {
    return undefined;
  }
  const hash = value.indexOf("#");
  if (hash !== -1) {
    const document = value.slice(0, hash);
    const target = parseBlockReference(document, value.slice(hash + 1));
    return target === undefined
      ? undefined
      : withBlock(parseNodeReference(document), target.block, target.pin);
  }
  const match = value.match(/^dispatch:\/\/([^/]+)(?:\/(.*))?$/);
  if (match === null) {
    return undefined;
  }
  const key = match[1];
  const target = match[2];
  if (key === undefined) {
    return undefined;
  }
  if (issueKeyOnlyPattern.test(key)) {
    return parseIssueReference(key, target);
  }
  if (key === "agent") {
    const artifact = target?.match(agentArtifactReferencePattern);
    if (artifact !== null && artifact !== undefined) {
      return agentArtifactRoute(artifact[1] ?? "", artifact[2] ?? "", artifact[3]);
    }
    // The session on its own is the agent itself; everything under it is what that agent's
    // conversation owns, which is a document and nothing else.
    return target !== undefined && isSessionId(target)
      ? { kind: "agent-session", session: target }
      : undefined;
  }
  if (key === "role") {
    return target !== undefined && roleTokenOnlyPattern.test(target)
      ? { kind: "role", role: target }
      : undefined;
  }
  if (key === "person") {
    return target !== undefined && personLoginOnlyPattern.test(target)
      ? { kind: "person", login: target }
      : undefined;
  }
  if (!projectKeyOnlyPattern.test(key) || target === undefined) {
    return undefined;
  }
  const component = target.match(componentReferencePattern);
  if (component !== null) {
    return { id: component[1] ?? "", kind: "component", project: key };
  }
  const document = target.match(projectArtifactReferencePattern);
  const itemID = document?.[4] === undefined ? undefined : decodedSegment(document[4]);
  if (document === null || (document[3] !== undefined && itemID === undefined)) {
    return undefined;
  }
  return projectDocumentRoute(
    key,
    document[1] ?? "",
    document[2],
    document[3] === undefined || itemID === undefined
      ? undefined
      : { id: itemID, kind: document[3] === "ask" ? "ask" : "comment" }
  );
}

/** The route of an agent's artifact page, `/agents/<session id>/artifacts/<slug>[?v=N]`; its
 *  segments are percent-decoded, and the session id then holds to the reference's rule. */
export function parseAgentArtifactPath(
  pathname: string,
  search = ""
): AgentArtifactRoute | undefined {
  const match = pathname.match(agentArtifactPathPattern);
  const session = match?.[1] === undefined ? undefined : decodedSegment(match[1]);
  if (match === null || session === undefined) {
    return undefined;
  }
  return agentArtifactRoute(session, match[2] ?? "", new URLSearchParams(search).get("v"));
}

/** The page of one machine's tmux layout (`features/machines/MachinePage.tsx`). The machine name
 *  is percent-encoded as a session id is, because it holds dots and may be a whole domain name.
 *  The builder lives in `features/refs` with the other paths the app builds, so a page links to
 *  a machine without importing `features/machines`; it is in this module and not in
 *  `page-routes.ts` because no module of the entry chunk reads it. */
export function buildMachinePath(host: string): string {
  return `/machines/${encodeURIComponent(host)}`;
}

/** The `?machine=` the Agents page filters on, named once here so the page reading it
 *  (`useAgentMachine`) and the links building it cannot drift apart. */
export const AGENTS_MACHINE_PARAM = "machine";

/** The `?role=` the Agents page narrows to, the page a `dispatch://role/<token>` reference
 *  opens (`buildReferencePath`), named here beside the filter it stands with. */
export const AGENTS_ROLE_PARAM = "role";

/** The `?component=` the Architecture pane holds its selected component in, the page a
 *  `dispatch://<PROJECT>/component/<id>` reference opens (`buildReferencePath`), named once
 *  here so the pane reading it (`features/architecture/ArchitecturePage.tsx`) and the link
 *  building it cannot drift apart. */
export const ARCHITECTURE_COMPONENT_PARAM = "component";

/** The Agents page narrowed to one machine, `/agents?machine=<host>`: the link a machine's own
 *  page sends a reader to its sessions by. */
export function buildAgentsMachinePath(host: string): string {
  return `/agents?${new URLSearchParams({ [AGENTS_MACHINE_PARAM]: host })}`;
}

/** The machine `/machines/<machine>` names, its segment percent-decoded as every other segment
 *  is (`decodedSegment`); `undefined` for any other path and for a segment naming nothing. */
export function parseMachinePath(pathname: string): string | undefined {
  const match = pathname.match(machinePathPattern);
  return match === null ? undefined : decodedSegment(match[1] ?? "");
}

/** The reference a `dispatch://` URL or a same-origin dashboard path names as text cites it, as
 * the server's `text.Extract` reads it: a composer's text, and a URL an author left as text in a
 * rendered body (`collectReferenceAnchors`); undefined for an external link or a dashboard path
 * that is not an issue/document reference (`pageRoutes`). A `#b-<block id>` fragment names the
 * page's block while text cites it (`pageInBound`), else the page; a reference past the bounds
 * names nothing (`parseDispatchReference`). */
export function referenceRouteFromHref(
  href: string,
  appOrigin: string = window.location.origin
): DispatchReferenceRoute | undefined {
  if (href.startsWith("dispatch://")) {
    return parseDispatchReference(href);
  }
  const routes = pageRoutes(href, appOrigin);
  return routes === undefined ? undefined : pageInBound(routes.page, routes.block);
}

/** The reference a link names as the server reads a request - a search hit's `href`, the target of
 * a link an author wrote, a `dispatch://` node reference - whatever the length of its names: the
 * bounds text is held to decide only what text cites (`referenceRouteFromHref`). */
export function nodeRouteFromHref(
  href: string,
  appOrigin: string = window.location.origin
): DispatchReferenceRoute | undefined {
  if (href.startsWith("dispatch://")) {
    return parseNodeReference(href);
  }
  const routes = pageRoutes(href, appOrigin);
  return routes?.block ?? routes?.page;
}

/** What a same-origin dashboard path names, as the server's `text` reads the page: `page`, and
 * `block`, the block of it its `#b-<block id>` fragment names, of any length. A path holding a
 * control character, naming a slug Dispatch never gives a document, or that is no issue/document
 * reference names nothing. A document page carrying `?comment=`/`?ask=` names that item whatever
 * its fragment, at any version of the document: the dashboard writes a document block's fragment
 * beside it, never a block of the item. A version of the spec (`?v=N`) is the spec's artifact at
 * N, as `dispatch read` reads the page. The fragment names a block of a live document, or of the
 * issue ask, comment or message its own page shows; no block of a document's version page, whose
 * stored text holds no block ids (`withBlock`). A raw `@` in a fragment is no pin, since pins are
 * written only in `dispatch://` references; any other fragment leaves the page. */
function pageRoutes(
  href: string,
  appOrigin: string
): { page: DispatchReferenceRoute; block: DispatchReferenceRoute | undefined } | undefined {
  if (hasControlCharacter(href)) {
    return undefined;
  }
  let url: URL;
  try {
    url = new URL(href, appOrigin);
  } catch {
    return undefined;
  }
  if (url.origin !== appOrigin) {
    return undefined;
  }
  const route =
    parseIssuePath(url.pathname, url.search) ??
    parseProjectPath(url.pathname, url.search) ??
    parseAgentArtifactPath(url.pathname, url.search);
  if (route === undefined || (isProjectRoute(route) && route.kind !== "document")) {
    return undefined;
  }
  // The Decisions tab is a view of the issue's decisions.md, so its page names that document,
  // whatever its fragment, as the server's `text` and the agent client read the page.
  if (route.kind === "decisions") {
    return {
      block: undefined,
      page: { key: route.key, kind: "artifact", slug: DECISIONS_DOCUMENT_SLUG },
    };
  }
  if ("slug" in route && !artifactSlugOnlyPattern.test(route.slug)) {
    return undefined;
  }
  // A document page carrying `?comment=`/`?ask=` names that item whatever its fragment, on an
  // issue's document as `parseProjectPath` already reads a project document's. Without it a
  // search hit's hover card previewed the document instead of the comment the reader is about to
  // open.
  if (route.kind === "document" && route.item !== undefined) {
    return { block: undefined, page: route };
  }
  if (!isProjectRoute(route) && (route.kind === "spec" || route.kind === "artifact")) {
    const item = itemFromSearch(url.search);
    const version = url.searchParams.get("v") ?? "";
    if (item === null) {
      return undefined;
    }
    if (item !== undefined) {
      return { block: undefined, page: { id: item.id, key: route.key, kind: item.kind } };
    }
    if (route.kind === "spec" && version !== "") {
      // `parseIssuePath` leaves the version to the spec's own page; a malformed one is the spec.
      return { block: undefined, page: artifactRoute(route.key, "spec", version) ?? route };
    }
  }
  const id = url.hash.startsWith("#b-") ? decodeBlockId(url.hash.slice(3)) : undefined;
  return { block: id === undefined ? undefined : withBlock(route, id), page: route };
}

export function buildDispatchReference(route: DispatchReferenceRoute): string {
  switch (route.kind) {
    case "agent-artifact":
      return `dispatch://agent/${route.session}/artifact/${encodeURIComponent(route.slug)}${
        route.version === undefined ? "" : `@v${route.version}`
      }`;
    // A party is written as it is read, its name needing no encoding: a session id carries none
    // of the characters a reference ends at, a role token is `[a-z0-9-]` and a login is a
    // lowercased email.
    case "agent-session":
      return `dispatch://agent/${route.session}`;
    case "role":
      return `dispatch://role/${route.role}`;
    case "person":
      return `dispatch://person/${route.login}`;
    case "component":
      return `dispatch://${route.project}/component/${route.id}`;
  }
  const target = "block" in route ? route.block : undefined;
  const block =
    target === undefined
      ? ""
      : `${blockFragment(target.id)}${target.pin === undefined ? "" : `@v${target.pin}`}`;
  if ("project" in route) {
    return `dispatch://${route.project}/artifact/${encodeURIComponent(route.slug)}${
      route.version === undefined ? "" : `@v${route.version}`
    }${route.item === undefined ? "" : `/${route.item.kind}/${encodeURIComponent(route.item.id)}`}${block}`;
  }
  const issue = `dispatch://${route.key}`;
  if (route.kind === "issue") {
    return issue;
  }
  if (route.kind === "spec") {
    return `${issue}/spec${block}`;
  }
  // An issue's tabs are the issue: the grammar lets only `spec`, `artifact/…`, `ask/…`,
  // `comment/…` and `message/…` follow an issue key, so the eight forms this once wrote -
  // verification, plan, steps, activity, children, artifacts, collaborators and the
  // conversation's `log` - were references the server refused to read.
  if (isPlainIssueRoute(route) || route.kind === "conversation") {
    return issue;
  }
  if (route.kind === "decisions") {
    return `${issue}/artifact/${DECISIONS_DOCUMENT_SLUG}`;
  }
  if (route.kind === "artifact") {
    return `${issue}/artifact/${encodeURIComponent(route.slug)}${
      route.version === undefined ? "" : `@v${route.version}`
    }${block}`;
  }
  return `${issue}/${route.kind}/${encodeURIComponent(route.id)}${block}`;
}

/** Whether `artifact` belongs to an agent's conversation (its `session_id` is set, its
 *  `issue_key` null and its `project` empty), so its page and reference sit under that agent's
 *  session rather than an issue or a project. */
export function isAgentArtifact<T extends Pick<Artifact, "session_id">>(
  artifact: T
): artifact is T & { readonly session_id: string } {
  return artifact.session_id !== undefined && artifact.session_id !== null;
}

/** The session id a `session:<id>` delivery address names, or undefined for any other address
 *  (a `role:` address). */
export function sessionOfAddress(address: string): string | undefined {
  return address.startsWith("session:") ? address.slice("session:".length) : undefined;
}

/** The party a delivery address names, written out kind by kind: `session:<id>` an agent's
 *  session and `role:<token>` a role, as the server's `model.ParseRoute` reads them
 *  (`internal/dispatch/model/model.go`), the grammar it holds every mention target to
 *  (`api.checkMentionRoute`). Undefined for an address of any other form, which no route
 *  reaches. A body can name a party outside this grammar - an agent reference takes the wider
 *  `SESSION_ID_PATTERN` - and such a party is reached by nothing, so the composer says so
 *  rather than listing it as one who is told (`party-references.ts`). A kind added to this
 *  grammar is named here or it is no party, rather than falling to whichever arm is last. */
export function partyOfAddress(address: string): PartyRoute | undefined {
  const session = sessionOfAddress(address);
  if (session !== undefined) {
    return sessionRouteIDPattern.test(session) ? { kind: "agent-session", session } : undefined;
  }
  if (address.startsWith("role:")) {
    const role = address.slice("role:".length);
    return roleTokenOnlyPattern.test(role) ? { kind: "role", role } : undefined;
  }
  return undefined;
}

/** The delivery address a party is reached at, or undefined for one no address reaches: a
 *  person, who reads the record and is never a delivering party (R5), and an agent whose
 *  session id is outside the grammar above. */
export function deliveryAddressOf(route: PartyRoute): string | undefined {
  switch (route.kind) {
    case "agent-session":
      return sessionRouteIDPattern.test(route.session) ? `session:${route.session}` : undefined;
    case "role":
      return roleTokenOnlyPattern.test(route.role) ? `role:${route.role}` : undefined;
    case "person":
      return undefined;
  }
}

/** The route a document is referenced and linked by: an issue's primary document is its `spec`,
 *  any other issue artifact is `artifact/<slug>`, a project document sits under its project, and
 *  an artifact of an agent's conversation under that agent's session. `version` pins a historical
 *  version (`@vN`), a form the bare `spec` route cannot carry, so a versioned primary document is
 *  referenced as `artifact/<slug>@vN`. */
export function documentRoute(
  artifact: Pick<Artifact, "issue_key" | "kind" | "primary" | "project" | "session_id" | "slug">,
  version?: number
): PagedReferenceRoute {
  if (isAgentArtifact(artifact)) {
    return version === undefined
      ? { kind: "agent-artifact", session: artifact.session_id, slug: artifact.slug }
      : { kind: "agent-artifact", session: artifact.session_id, slug: artifact.slug, version };
  }
  if (artifact.issue_key === null) {
    return version === undefined
      ? { kind: "document", project: artifact.project, slug: artifact.slug }
      : { kind: "document", project: artifact.project, slug: artifact.slug, version };
  }
  if (version === undefined && artifact.primary && artifact.kind === "doc") {
    return { key: artifact.issue_key, kind: "spec" };
  }
  return version === undefined
    ? { key: artifact.issue_key, kind: "artifact", slug: artifact.slug }
    : { key: artifact.issue_key, kind: "artifact", slug: artifact.slug, version };
}

/** The artifact a reference names: its owner, its slug, and the version it pins when it pins
 *  one. */
export interface ReferencedArtifact {
  readonly owner: ArtifactOwner;
  readonly slug: string;
  readonly version: number | undefined;
}

/** The artifact a reference names, as its owner, its slug and the version it pins: an issue's
 *  artifact, a project document (not an ask, a comment or a block of one), or an agent
 *  conversation's artifact. Undefined for a reference to anything else. */
export function referencedArtifact(route: DispatchReferenceRoute): ReferencedArtifact | undefined {
  if ("block" in route && route.block !== undefined) {
    return undefined;
  }
  switch (route.kind) {
    case "artifact":
      return { owner: { issue: route.key }, slug: route.slug, version: route.version };
    case "document":
      return route.item === undefined
        ? { owner: { project: route.project }, slug: route.slug, version: route.version }
        : undefined;
    case "agent-artifact":
      return { owner: { session: route.session }, slug: route.slug, version: route.version };
    default:
      return undefined;
  }
}

/** The title a link gives the issue or the whole document `route` names, from what the reads
 *  hold of them (R8, `reference-target.ts`'s `useReferenceTarget`): a document by its own name,
 *  which is its file name, and an issue and its spec by the issue's title. A reference that
 *  names a document is titled by that document alone: one its issue does not hold has no title,
 *  never its issue's, which would name a node the reference does not name. The issue page
 *  publishes a whole document's name by this rule (`issue-page/tree-drafts.ts`'s
 *  `treeContext`), so a line its batched read feeds names the document as a link to it does. */
export function issueOrDocumentTitle(
  route: DispatchReferenceRoute | undefined,
  issue: { readonly title: string } | undefined,
  document: { readonly name: string } | undefined
): string | undefined {
  return route !== undefined && referencedArtifact(route) !== undefined
    ? document?.name
    : issue?.title;
}

/** What a picture whose source is a Dispatch reference names: the source as written, the route it
 *  parses to, the artifact that route names (`referencedArtifact`, undefined for an ask, a comment
 *  or an issue) and the caption to draw it with. */
export interface PictureReference {
  readonly caption: string;
  readonly named: ReferencedArtifact | undefined;
  readonly route: DispatchReferenceRoute;
  readonly source: string;
}

/** The one reading of an `image` node's attributes as a Dispatch picture, shared by the engine's
 *  `pictureSerializer` (`markdown-engine.ts`) and the editors' `pictureView`
 *  (`doc/picture-view.ts`) so a picture in a comment and the same picture in a spec are read by
 *  one rule. The source is `src` as a string (`""` when absent); it is a reference only when it
 *  starts with `dispatch://` and the grammar reads it, else undefined and the picture is whatever
 *  the caller draws for any other address. An author names the picture's node explicitly, and
 *  drawing it is a read, so the source is read as a node's reference (`parseNodeReference`),
 *  whatever the length of its name. The caption is the trimmed `alt`, or the slug of the
 *  artifact the reference names when the `alt` is blank (`""` when it names no artifact). */
export function pictureReference(attrs: {
  readonly alt?: unknown;
  readonly src?: unknown;
}): PictureReference | undefined {
  const source = String(attrs.src ?? "");
  const route = source.startsWith("dispatch://") ? parseNodeReference(source) : undefined;
  if (route === undefined) {
    return undefined;
  }
  const named = referencedArtifact(route);
  const alt = String(attrs.alt ?? "").trim();
  return { caption: alt === "" ? (named?.slug ?? "") : alt, named, route, source };
}

/** The route an ask or comment is referenced by: under its issue when it has one, else under the
 *  project document that owns it. Only Inbox rows carry `document`, so a caller showing a
 *  project document's items names that document; without either there is no reference. */
export function itemRoute(
  kind: "ask" | "comment",
  item: { id: string; issue_key: string | null },
  document: { project: string; slug: string } | undefined
): PagedReferenceRoute | undefined {
  if (item.issue_key !== null) {
    return { id: item.id, key: item.issue_key, kind };
  }
  if (document === undefined) {
    return undefined;
  }
  return {
    item: { id: item.id, kind },
    kind: "document",
    project: document.project,
    slug: document.slug,
  };
}

/** The path that opens `artifact` with `item` in view: the document's own route naming the item,
 *  the shape every in-app thread link already uses. A typed ask block is named by its block
 *  fragment, which is how a decision block is focused, rather than by the ask's id. */
export function documentItemPath(
  artifact: Pick<Artifact, "issue_key" | "kind" | "primary" | "project" | "session_id" | "slug">,
  item: { blockID?: string; id: string; kind: "ask" | "comment" }
): string {
  const path = buildReferencePath(documentRoute(artifact));
  if (item.blockID !== undefined) {
    return `${path}${blockFragment(item.blockID)}`;
  }
  return `${path}?${item.kind}=${encodeURIComponent(item.id)}`;
}

/** A reference's compact plain-text form: what its link shows until the target's title
 * resolves (or if it never does, e.g. a deleted ask), and what a plain-text headline
 * (`useMarkdownHeadline`) writes in its place. Mirrors `buildDispatchReference`'s shape without
 * the `dispatch://` scheme, so it reads like a second, shorter reference. */
export function shortForm(route: DispatchReferenceRoute): string {
  switch (route.kind) {
    case "agent-artifact":
      return `agent/${shortSessionId(route.session)}/${route.slug}`;
    case "agent-session":
      return `agent/${shortSessionId(route.session)}`;
    case "role":
      return `role/${route.role}`;
    case "person":
      return `person/${route.login}`;
    case "component":
      return `${route.project}/component/${route.id}`;
  }
  const target = "block" in route ? route.block : undefined;
  const block =
    target === undefined ? "" : ` block${target.pin === undefined ? "" : ` v${target.pin}`}`;
  if (isProjectRoute(route)) {
    const base = `${route.project}/${route.slug}`;
    return route.item === undefined ? `${base}${block}` : `${base} ${route.item.kind}${block}`;
  }
  if (isPlainIssueRoute(route)) {
    return `${route.key} ${route.kind}`;
  }
  switch (route.kind) {
    case "issue":
      return route.key;
    case "spec":
      return `${route.key} spec${block}`;
    case "conversation":
      return `${route.key} log`;
    case "decisions":
      return `${route.key} decisions`;
    case "artifact":
      return `${route.key} ${route.slug}${block}`;
    case "ask":
      return `${route.key} ask${block}`;
    case "comment":
      return `${route.key} comment${block}`;
    case "message":
      return `${route.key} message${block}`;
  }
}

/**
 * The `href` a reference's path becomes inside this dashboard, or `undefined` for a path that
 * is no link of this dashboard's. It is the one thing written into an `href` built from a
 * reference a person typed (`RefLink`'s two writers, `features/doc/picture-view`).
 *
 * A path qualifies on three conditions, because any one alone lets a link off the site: it
 * begins with exactly one slash, so `//evil.example/x`, `javascript:` and `data:` are out; it
 * still names the site it is resolved against once the URL parser has read it, so
 * `/\evil.example/x`, which a leading-slash check passes and every browser sends to
 * evil.example, is out too; and what the parser read still begins with exactly one slash,
 * since reading the dot segments out of `/.//evil.example/x`, `/a/..//evil.example` or
 * `/%2e//evil.example` puts back the two the first condition refused.
 *
 * What it answers is built from the parsed URL's own parts rather than handed back as it came,
 * so the string written into the attribute carries no scheme and no host whatever the path
 * held: there is no spelling of the input that becomes one.
 *
 * The answer does not depend on which site serves the dashboard: a path that stays on one
 * origin stays on every origin, and an absolute URL, this dashboard's own included, is already
 * out by the leading slash. So the parser is given a base of its own rather than
 * `window.location.origin`, which is `null` at an opaque origin and no base at all.
 *
 * Every path `buildReferencePath` writes qualifies today, each segment encoded or matched
 * against a key pattern. It is the gate all the same, at each sink rather than at the builder,
 * so no later arm of that switch, and no other route a reference resolves to, can reach an
 * `href` without passing here.
 */
export function appHref(path: string): string | undefined {
  if (!path.startsWith("/") || path.startsWith("//")) {
    return undefined;
  }
  let url: URL;
  try {
    url = new URL(path, SAME_SITE_PROBE_ORIGIN);
  } catch {
    return undefined;
  }
  if (url.origin !== SAME_SITE_PROBE_ORIGIN) {
    return undefined;
  }
  const href = `${url.pathname}${url.search}${url.hash}`;
  // Reading the dot segments out puts back the `//` the check above refused: `/.//evil.com/x`
  // parses to the path `//evil.com/x`, which a browser resolving this attribute sends to
  // evil.com. The answer is held to the same two slashes the input was.
  return href.startsWith("//") ? undefined : href;
}

/** The base `appHref` reads a path against: a syntactically valid origin that is no real site,
 *  so a path that escapes it escapes any origin and nothing is ever fetched from it. */
const SAME_SITE_PROBE_ORIGIN = "https://same-site.invalid";

/** The `href` a reference opens inside this dashboard, or `undefined` for one that opens no
 *  page of it: the path the reference builds (`buildReferencePath`), through the gate above.
 *  The composition every sink of a typed reference makes - `RefLink`'s two writers,
 *  `features/doc/picture-view` and the two hooks of `reference-target.ts` - named once here,
 *  beside the gate it is made of. `specSlug` is `buildReferencePath`'s, the slug a pinned block
 *  of an issue's spec opens its version under, which only the issue's summary names. */
export function referenceHref(
  route: DispatchReferenceRoute,
  specSlug?: string
): string | undefined {
  // A person has no page, so there is no path to gate: the sink draws the words instead.
  return route.kind === "person" ? undefined : appHref(buildReferencePath(route, specSlug));
}

/** The SPA path a reference navigates to, whichever side of the issue/project split it is on. A
 *  block opens its document, issue comment or message at the block (`#b-`); a pinned block of a
 *  document opens the version of its document it pins, since a version's stored text holds no
 *  block ids to land on, the pin's digits written as the reference holds them, and a pinned block
 *  of a comment opens the live comment at the block, a comment's revisions having no page of
 *  their own. A block of a project document's comment opens the comment (`?comment=<id>`) with no
 *  fragment: that page's `#b-` names a block of the document. An issue's spec keeps its versions
 *  under its own slug, which a spec route does not carry: `specSlug` names it once the issue's
 *  summary is read (`useReferenceHref`), and until then a pinned block of the spec opens the live
 *  spec at the block. A person is no caller of this: nothing shows one, so a person's reference
 *  opens no page, and leaving that kind out (`PagedReferenceRoute`) is what makes each sink
 *  that can hold one say for itself what it draws instead (R5). */
export function buildReferencePath(route: PagedReferenceRoute, specSlug?: string): string {
  switch (route.kind) {
    case "agent-artifact": {
      const artifact = `/agents/${encodeURIComponent(route.session)}/artifacts/${encodeURIComponent(route.slug)}`;
      return route.version === undefined ? artifact : `${artifact}?v=${route.version}`;
    }
    case "agent-session":
      return `/agents/${encodeURIComponent(route.session)}/live`;
    case "role":
      return `/agents?${new URLSearchParams({ [AGENTS_ROLE_PARAM]: route.role })}`;
    case "component": {
      const architecture = buildProjectPath({ kind: "architecture", project: route.project });
      return `${architecture}?${ARCHITECTURE_COMPONENT_PARAM}=${encodeURIComponent(route.id)}`;
    }
  }
  const path = isProjectRoute(route) ? buildProjectPath(route) : buildIssuePath(route);
  const target = "block" in route ? route.block : undefined;
  if (target === undefined || (route.kind === "document" && route.item !== undefined)) {
    return path;
  }
  const live = `${path}${blockFragment(target.id)}`;
  if (target.pin === undefined || itemOfBlock(route) !== undefined) {
    return live;
  }
  if (route.kind === "document") {
    const document = buildProjectPath({
      kind: "document",
      project: route.project,
      slug: route.slug,
    });
    return `${document}?version=${target.pin}`;
  }
  const slug = route.kind === "artifact" ? route.slug : specSlug;
  return slug === undefined
    ? live
    : `${buildIssuePath({ key: route.key, kind: "artifact", slug })}?v=${target.pin}`;
}

/** The values a filter field accepts, or undefined for the one field whose vocabulary is the
 *  workspace's own: a project key is whatever somebody created, so only `none` is reserved. */
function filterVocabulary(field: InboxFilterField): readonly string[] | undefined {
  if (field === "priority") return INBOX_PRIORITY_VALUES;
  if (field === "urgency") return INBOX_URGENCY_VALUES;
  return field === "type" ? INBOX_TYPE_VALUES : undefined;
}

/**
 * The page's `?assignee=` deliberately spells its default the opposite way round from the
 * server's own `?assignee=` on `GET /api/v1/inbox`
 * (`packages/envoy/internal/dispatch/api/inbox.go`), where an absent parameter means everybody:
 * here absent means the reader alone and `assignee=everyone` means everybody, so the same name
 * now means different things on the two sides and neither reads the other's URL.
 *
 * Every other field is absent while it holds its default, so the Inbox as it opens is `/` and a
 * link says only what the reader changed. A value this build does not know - an older link, a
 * typo - is dropped rather than refused: the list is a client-side split of what the two reads
 * answered, so an unknown word would match no row anyway.
 */
export function parseInboxSearch(search: string): InboxRoute {
  const params = new URLSearchParams(search);
  const agent = params.get("agent");
  const route: InboxRoute = {};
  if (agent !== null && agent !== "") {
    route.agent = agent;
  }
  const item = params.get("item")?.trim() ?? "";
  if (item !== "") {
    route.item = item;
  }
  if (params.get("section") === "needs-you") {
    route.section = "needs-you";
  }
  // Neither trimmed nor cut: a reader mid-word has a trailing space that is part of what they
  // typed, and the box rewrites no address a link arrived with.
  const q = params.get("q");
  if (q !== null && q !== "") {
    route.q = q;
  }
  // Nothing refuses an email the page has never heard of: the filter is a client-side split of
  // one inbox response, so an unlisted or misspelled person simply matches no row.
  const assignees: string[] = [];
  let everyone = false;
  for (const raw of params.getAll("assignee")) {
    const value = raw.trim().toLowerCase();
    if (value === "") continue;
    if (value === EVERYONE_FILTER_VALUE) {
      everyone = true;
      continue;
    }
    if (!assignees.includes(value)) assignees.push(value);
  }
  if (assignees.length > 0 || everyone) {
    route.assignees = assignees;
  }
  if (params.get("show") === "dismissed") {
    route.show = "dismissed";
  }
  const sort = params.get("sort");
  if (sort !== null && sort !== "newest" && (INBOX_SORTS as readonly string[]).includes(sort)) {
    route.sort = sort as InboxSort;
  }
  const group = params.get("group");
  if (
    group !== null &&
    group !== "priority" &&
    (INBOX_GROUP_BYS as readonly string[]).includes(group)
  ) {
    route.group = group as InboxGroupBy;
  }
  const filters: Partial<Record<InboxFilterField, readonly string[]>> = {};
  for (const field of INBOX_FILTER_FIELDS) {
    const vocabulary = filterVocabulary(field);
    const chosen: string[] = [];
    for (const raw of params.getAll(field)) {
      for (const part of raw.split(",")) {
        const value = part.trim();
        if (value === "" || chosen.includes(value)) continue;
        if (vocabulary !== undefined && !vocabulary.includes(value)) continue;
        chosen.push(value);
      }
    }
    if (chosen.length > 0) filters[field] = chosen;
  }
  if (Object.keys(filters).length > 0) {
    route.filters = filters;
  }
  return route;
}
