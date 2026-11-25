/**
 * The addresses of the dashboard's pages as the shell reads them: the router's path patterns,
 * the parse and the build of an issue's and a project's path, the Inbox's path, and the tests
 * the shell and the rail make on a pathname.
 *
 * WHY A MODULE OF ITS OWN. The entry chunk reads these addresses before it draws anything: the
 * router, the rail, the margin's column, the landing read and the recent items. The entry is
 * what every page parses first (`web/src/asset-sizes-in-build.ts` holds it to its budget), and
 * rollup puts all of a module in one chunk. So the grammar of `dispatch://` references and the
 * addresses no module of the entry reads are in `routes.ts`, which imports this module, and only
 * the chunks of the pages and of the reference surfaces load it. This module reads two Inbox
 * types from `routes.ts` (`InboxGroupBy`, `InboxSort`), as types only, so no code of `routes.ts`
 * comes into the entry.
 */
import { hasControlCharacter, itemFromSearch } from "@legion/contracts/dispatch-href";
import { PLAIN_ISSUE_TABS } from "@legion/contracts/dispatch-text-rules";
import { matchPath } from "react-router-dom";

import type { Artifact } from "../../api/types";
import { issueProject } from "../../lib/issue-key";
import { EVERYONE_FILTER_VALUE } from "../inbox/sections";
import type { BlockTarget } from "./block-reference";
import type { InboxGroupBy, InboxSort } from "./routes";

export type IssueRoute =
  | { key: string; kind: "issue" }
  | ({ key: string; kind: "spec" } & BlockTarget)
  | { key: string; kind: "verification" }
  | { key: string; kind: "plan" }
  | { key: string; kind: "steps" }
  | { key: string; kind: "conversation" }
  | { key: string; kind: "activity" }
  | { key: string; kind: "decisions" }
  | { key: string; kind: "children" }
  | { key: string; kind: "artifacts" }
  | { key: string; kind: "collaborators" }
  | ({ key: string; kind: "artifact"; slug: string; version?: number } & BlockTarget)
  | ({ key: string; kind: "ask"; id: string } & BlockTarget)
  | ({ key: string; kind: "comment"; id: string } & BlockTarget)
  | ({ key: string; kind: "message"; id: string } & BlockTarget);

/** `project` is the bare `/projects/:key`, which opens on `architecture` when the project has
 *  an architecture source and on `issues` otherwise (`ProjectPage`'s open-on rule); the Issues
 *  tab and its `?label=`/`?status=`/`?q=` filter URLs live at `/issues`. */
export type ProjectRoute =
  | { kind: "project"; project: string }
  | { kind: "architecture"; project: string }
  | { kind: "issues"; project: string }
  | { kind: "documents"; project: string }
  | ({
      kind: "document";
      project: string;
      slug: string;
      version?: number;
      item?: { kind: "ask" | "comment"; id: string };
    } & BlockTarget);

export type DispatchRoute = IssueRoute | ProjectRoute;
export type ProjectDocumentRoute = Extract<ProjectRoute, { kind: "document" }>;

/** The four dimensions whose chosen values are comma lists in the address. */
export const INBOX_FILTER_FIELDS = ["priority", "urgency", "type", "project"] as const;
export type InboxFilterField = (typeof INBOX_FILTER_FIELDS)[number];

/** The Inbox at `/`, optionally narrowed to one agent's asks (`?agent=<session id>`); with
 *  `section=needs-you` only the asks waiting on the viewer are shown. Every field below is
 *  absent while it holds its default, so a plain `/` is the Inbox as it opens. */
export interface InboxRoute {
  agent?: string;
  /** The row the Inbox has selected, the one its reading pane shows: an ask's id, or a thread's
   *  `reply:<thread id>` / `agent:<message id>` (`rowId`, `features/inbox/list-row.ts`). The
   *  address is the whole memory of that selection, so a link carries the reader's place. */
  item?: string;
  section?: "needs-you";
  /** The text the Inbox's search box narrows the list by, as typed. It is a field of the route
   *  rather than a search parameter of its own because `buildInboxPath` rebuilds the whole query
   *  string from this type and `usePaneSelection` navigates with it on every selection change, so
   *  a parameter outside the type would be dropped by the next `j`. */
  q?: string;
  /** Who the listed asks are for. Absent is the page's default, the reader's own work; an empty
   *  array is the cleared filter, every question; a value is a person's lowercase email or the
   *  literal `unassigned`. */
  assignees?: readonly string[];
  /** `?show=dismissed`, the View menu's **Show** switch on **Dismissed**: what the reader has
   *  dismissed out of their Inbox, newest dismissal first. Absent is the Inbox itself, every
   *  undismissed row, read or not. */
  show?: "dismissed";
  sort?: InboxSort;
  group?: InboxGroupBy;
  /** The chosen values of each filter field, one comma list per field in the address. A field
   *  with nothing chosen is absent, never an empty array. */
  filters?: Partial<Record<InboxFilterField, readonly string[]>>;
}
export type IssueTab =
  | "spec"
  | "verification"
  | "plan"
  | "steps"
  | "conversation"
  | "children"
  | "artifacts"
  | "activity"
  | "decisions"
  | "collaborators";

/** An issue tab whose whole route is the tab itself (`@legion/contracts`'s `PLAIN_ISSUE_TABS`,
 *  which the server reads the same way): no block, no slug, no id. Each one is parsed from the
 *  segment of its own name, built as `<issue>/<kind>`, shown as `<KEY> <kind>` and is the tab of
 *  its own name, so the chains here and in `routes.ts` hold the seven in one arm rather than one
 *  apiece and an eighth tab is that list. The spec, the conversation and the decisions stay out
 *  of it: a spec route carries a block, the conversation is written `log` in a reference and
 *  `conversation` in a path, and the Decisions tab is a view of the issue's `decisions.md`,
 *  which a reference names as that document. */
type PlainIssueTab = (typeof PLAIN_ISSUE_TABS)[number];

/** The plain tab a path segment names, or `undefined` for anything else. */
export function plainIssueTab(value: string): PlainIssueTab | undefined {
  return PLAIN_ISSUE_TABS.find((tab) => tab === value);
}

type PlainIssueRoute = Extract<IssueRoute, { kind: PlainIssueTab }>;

/** Whether the route is one of those tabs, narrowing it for the builders here and in
 *  `routes.ts` and leaving the rest of the union to the arms that follow. */
export function isPlainIssueRoute(route: IssueRoute): route is PlainIssueRoute {
  return PLAIN_ISSUE_TABS.some((tab) => tab === route.kind);
}

/** A project's key and an issue's key, which a path and a reference write alike. */
export const projectKeyPattern = "[A-Z][A-Z0-9]{1,9}";
export const issueKeyPattern = `${projectKeyPattern}-[1-9]\\d*`;
const issuePathPattern = new RegExp(`^/issues/(${issueKeyPattern})(?:/(.*))?$`);
const projectPathPattern = new RegExp(`^/projects/(${projectKeyPattern})(?:/(.*))?$`);

/** The page of an agent's artifact, the router's pattern for `AgentArtifactRoute`. */
export const AGENT_ARTIFACT_PATH = "/agents/:sessionId/artifacts/:slug";

export function isPrimaryDocumentArtifactRoute(
  route: IssueRoute,
  artifact: Pick<Artifact, "kind" | "primary"> | undefined
): boolean {
  return route.kind === "artifact" && artifact?.primary === true && artifact.kind === "doc";
}

export function issueTabForRoute(
  route: IssueRoute,
  artifact: Pick<Artifact, "kind" | "primary"> | undefined
): IssueTab {
  if (isPlainIssueRoute(route)) {
    return route.kind;
  }
  if (route.kind === "conversation") {
    return "conversation";
  }
  if (route.kind === "decisions") {
    return "decisions";
  }
  if (route.kind === "artifact") {
    return isPrimaryDocumentArtifactRoute(route, artifact) ? "spec" : "artifacts";
  }
  return route.kind === "issue" || route.kind === "spec" ? "spec" : "conversation";
}

/** A path segment decoded; `undefined` for one that does not decode, decodes empty, or decodes to
 * a control character, which no id or slug holds and the server's `text.Extract` refuses. */
export function decodedSegment(value: string): string | undefined {
  try {
    const decoded = decodeURIComponent(value);
    return decoded === "" || hasControlCharacter(decoded) ? undefined : decoded;
  } catch {
    return undefined;
  }
}

function version(value: string | null | undefined): number | undefined | null {
  if (value === null || value === undefined || value === "") {
    return undefined;
  }
  return /^[1-9]\d*$/.test(value) ? Number(value) : null;
}

/** A route's slug percent-decoded and its version parsed; `undefined` when either names nothing. */
export function decodedSlugAndVersion(
  slug: string,
  versionValue: string | null | undefined
): { slug: string; version: number | undefined } | undefined {
  const decodedSlug = decodedSegment(slug);
  const parsedVersion = version(versionValue);
  return decodedSlug === undefined || parsedVersion === null
    ? undefined
    : { slug: decodedSlug, version: parsedVersion };
}

export function artifactRoute(
  key: string,
  slug: string,
  versionValue?: string | null
): IssueRoute | undefined {
  const parsed = decodedSlugAndVersion(slug, versionValue);
  if (parsed === undefined) {
    return undefined;
  }
  return parsed.version === undefined
    ? { key, kind: "artifact", slug: parsed.slug }
    : { key, kind: "artifact", slug: parsed.slug, version: parsed.version };
}

export function projectDocumentRoute(
  project: string,
  slug: string,
  versionValue: string | null | undefined,
  item?: { kind: "ask" | "comment"; id: string }
): ProjectDocumentRoute | undefined {
  const parsed = decodedSlugAndVersion(slug, versionValue);
  if (parsed === undefined) {
    return undefined;
  }
  return {
    ...(item === undefined ? {} : { item }),
    kind: "document",
    project,
    slug: parsed.slug,
    ...(parsed.version === undefined ? {} : { version: parsed.version }),
  };
}

export function parseIssuePath(pathname: string, search = ""): IssueRoute | undefined {
  const match = pathname.match(issuePathPattern);
  if (match === null) {
    return undefined;
  }
  const key = match[1];
  const target = match[2];
  if (key === undefined) {
    return undefined;
  }
  if (target === undefined) {
    return { key, kind: "issue" };
  }
  if (target === "spec") {
    return { key, kind: "spec" };
  }
  const plain = plainIssueTab(target);
  if (plain !== undefined) {
    return { key, kind: plain };
  }
  if (target === "conversation") {
    return { key, kind: "conversation" };
  }
  if (target === "decisions") {
    return { key, kind: "decisions" };
  }
  const artifact = target.match(/^artifacts?\/([^/?#\s]+)$/);
  if (artifact !== null) {
    return artifactRoute(key, artifact[1] ?? "", new URLSearchParams(search).get("v"));
  }
  const item = target.match(/^(asks|comments|messages)\/([^/?#\s]+)$/);
  const id = item?.[2] === undefined ? undefined : decodedSegment(item[2]);
  if (item === null || id === undefined) {
    return undefined;
  }
  return {
    id,
    key,
    kind: item[1] === "asks" ? "ask" : item[1] === "comments" ? "comment" : "message",
  };
}

export function parseProjectPath(pathname: string, search = ""): ProjectRoute | undefined {
  const match = pathname.match(projectPathPattern);
  if (match === null) {
    return undefined;
  }
  const project = match[1];
  const target = match[2];
  if (project === undefined) {
    return undefined;
  }
  if (target === undefined) {
    return { kind: "project", project };
  }
  if (target === "architecture") {
    return { kind: "architecture", project };
  }
  if (target === "issues") {
    return { kind: "issues", project };
  }
  if (target === "documents") {
    return { kind: "documents", project };
  }
  const document = target.match(/^documents\/([^/?#\s]+)$/);
  if (document === null) {
    return undefined;
  }
  const item = itemFromSearch(search);
  if (item === null) {
    return undefined;
  }
  return projectDocumentRoute(
    project,
    document[1] ?? "",
    new URLSearchParams(search).get("version"),
    item
  );
}

/** The project document a page's path names, and undefined for every other path: the document
 *  whose name the search bar's chip says (`features/search/useBarScope.tsx`), and the one a visit
 *  records as a recent item (`features/search/recents.ts`). It is here and not with the chip's
 *  other rules (`features/search/bar-scope.ts`): the recent items are in the entry chunk, and
 *  those rules are not. */
export function documentRouteOf(
  pathname: string,
  search: string
): ProjectDocumentRoute | undefined {
  const route = parseProjectPath(pathname, search);
  return route?.kind === "document" ? route : undefined;
}

/** The project a route is about: a project path's own key, or an issue path's key without its
 *  number (`issueProject`, the one key-to-project rule); `undefined` elsewhere. */
export function routeProjectOf(pathname: string): string | undefined {
  const issueKey = parseIssuePath(pathname)?.key;
  return (
    parseProjectPath(pathname)?.project ??
    (issueKey === undefined ? undefined : issueProject(issueKey))
  );
}

export function buildIssuePath(route: IssueRoute): string {
  // Encoded although an issue key is `[A-Z][A-Z0-9]{1,9}-<digits>` and so already a path
  // segment: a key that reached this builder out of a reference a person typed has been read
  // by a pattern, never trusted, and encoding says so where the path is built rather than at
  // each of the places one is written into an `href`.
  const issue = `/issues/${encodeURIComponent(route.key)}`;
  if (route.kind === "issue") {
    return issue;
  }
  if (route.kind === "spec") {
    return `${issue}/spec`;
  }
  if (isPlainIssueRoute(route)) {
    return `${issue}/${route.kind}`;
  }
  if (route.kind === "conversation") {
    return `${issue}/conversation`;
  }
  if (route.kind === "decisions") {
    return `${issue}/decisions`;
  }
  if (route.kind === "artifact") {
    const artifact = `${issue}/artifacts/${encodeURIComponent(route.slug)}`;
    return route.version === undefined ? artifact : `${artifact}?v=${route.version}`;
  }
  const segment =
    route.kind === "ask" ? "asks" : route.kind === "comment" ? "comments" : "messages";
  return `${issue}/${segment}/${encodeURIComponent(route.id)}`;
}

export function buildProjectPath(route: ProjectRoute): string {
  // Encoded for the reason `buildIssuePath` gives: a project key is `[A-Z][A-Z0-9]{1,9}`, so
  // this is identity for every key the server makes, and the path carries no caller's
  // characters whatever reached it.
  const project = `/projects/${encodeURIComponent(route.project)}`;
  if (route.kind === "project") {
    return project;
  }
  if (route.kind === "architecture" || route.kind === "issues" || route.kind === "documents") {
    return `${project}/${route.kind}`;
  }
  const document = `${project}/documents/${encodeURIComponent(route.slug)}`;
  const query = new URLSearchParams();
  if (route.version !== undefined) {
    query.set("version", String(route.version));
  }
  if (route.item !== undefined) {
    query.set(route.item.kind, route.item.id);
  }
  const search = query.toString();
  return search === "" ? document : `${document}?${search}`;
}

export function buildInboxPath(route: InboxRoute = {}): string {
  const query = new URLSearchParams();
  if (route.agent !== undefined) {
    query.set("agent", route.agent);
  }
  if (route.item !== undefined) {
    query.set("item", route.item);
  }
  if (route.section !== undefined) {
    query.set("section", route.section);
  }
  if (route.q !== undefined) {
    query.set("q", route.q);
  }
  if (route.assignees !== undefined) {
    if (route.assignees.length === 0) {
      query.append("assignee", EVERYONE_FILTER_VALUE);
    }
    for (const assignee of route.assignees) {
      query.append("assignee", assignee);
    }
  }
  if (route.show !== undefined) {
    query.set("show", route.show);
  }
  if (route.sort !== undefined && route.sort !== "newest") {
    query.set("sort", route.sort);
  }
  if (route.group !== undefined && route.group !== "priority") {
    query.set("group", route.group);
  }
  for (const field of INBOX_FILTER_FIELDS) {
    const chosen = route.filters?.[field];
    if (chosen !== undefined && chosen.length > 0) query.set(field, chosen.join(","));
  }
  const search = query.toString();
  return search === "" ? "/" : `/?${search}`;
}

/**
 * Whether this route has a margin at all: an issue, or a standalone project document. Nothing
 * else does, so the shell reserves no gutter for one and `features/margin/Margin.tsx` renders
 * nothing there. One helper because two answers that disagree leave an 80 px gutter beside a
 * column that is not there. It answers from the route rather than from the resolved margin
 * owner, whose document form only appears once the artifact query lands.
 */
export function routeHasMargin(pathname: string, search = ""): boolean {
  return (
    parseIssuePath(pathname, search) !== undefined ||
    parseProjectPath(pathname, search)?.kind === "document"
  );
}

/** Whether this route is the Inbox page itself, the one place a peek at it has nothing to add.
 *  `app.tsx` renders `InboxPage` at `/credentials/*` as well, for an address kept from before
 *  Dispatch's credential record page was deleted, and ranks `/credentials/machine`, a page of its
 *  own, above that splat, as this does. */
export function routeIsInbox(pathname: string): boolean {
  return (
    pathname === "/" ||
    (matchPath("/credentials/*", pathname) !== null &&
      matchPath("/credentials/machine", pathname) === null)
  );
}

/** The signed-in person's answers and ask replies (`AnswersPage`), linked from the Inbox page. */
export const ANSWERS_PATH = "/answers";

/** One person's page (`PersonPage`), linked from the signed-in login in the rail. */
export const PERSON_PATH = "/people/:login";

/** The page of the person this login names. */
export function buildPersonPath(login: string): string {
  return `/people/${encodeURIComponent(login)}`;
}

/** The live agent view's route pattern, shared by the router and `routeFillsViewport`. */
export const AGENT_LIVE_PATH = "/agents/:sessionId/live";

/**
 * Whether this route's page owns its scroller whatever the viewport, and so gets exactly the
 * viewport from the shell: the live agent view, whose thread scrolls while its header and
 * composer stay put. The Inbox joins it only while its reading pane shows (`app.tsx` pairs this
 * with `READING_PANE_QUERY`); every other page scrolls the document, which `ViewportAnchor` and
 * the sticky composers depend on.
 */
export function routeFillsViewport(pathname: string): boolean {
  return matchPath(AGENT_LIVE_PATH, pathname) !== null;
}

/** The session whose live view `pathname` names, and `undefined` on every other address. The
 *  address is read here, beside the pattern, because a surface outside this module holds no
 *  live route of its own: its link into a live view is `AgentName`'s (`biome.json`). */
export function liveViewSessionOf(pathname: string): string | undefined {
  return matchPath(AGENT_LIVE_PATH, pathname)?.params.sessionId;
}
