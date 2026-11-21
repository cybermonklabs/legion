import {
  type QueryClient,
  type QueryKey,
  QueryObserver,
  type QueryObserverOptions,
  type QueryObserverResult,
  type QueryStatus,
  queryOptions,
  type UseQueryOptions,
  type UseQueryResult,
  useQueries,
  useQuery,
} from "@tanstack/react-query";
import { useContext } from "react";

import { ApiError, api, artifactVersionPath } from "../../api/client";
import { artifactAtAddress, primarySpec } from "../../api/issue-cache";
import { issueCollaboratorsQuery, issueQuery, usersQuery } from "../../api/queries";
import type {
  ArchitectureComponentRead,
  Artifact,
  ArtifactText,
  ArtifactVersionContent,
  AskRead,
  CommentRead,
  DisplayTree,
  GraphReferences,
  IssueReferenceSummary,
  IssueReferenceSummaryDocument,
  MessageRead,
  ReferenceQuestions,
} from "../../api/types";
import {
  agentsQuery,
  EMPTY_AGENTS,
  registeredAgent,
  registeredTitle,
  useRegistryPolling,
} from "./agent-registry";
import { itemOfBlock } from "./block-reference";
import { knownSessionQuery } from "./known-sessions";
import { type ReferenceNaming, useMarkdownHeadline } from "./markdown-engine";
import { RecordIssue, recordedRoles, roleHolders } from "./role-holders";
import {
  buildDispatchReference,
  type DispatchReferenceRoute,
  type IssueRoute,
  issueOfRoute,
  type ReferencedArtifact,
  referencedArtifact,
  referenceHref,
} from "./routes";
import { referenceTargetKind } from "./target-id";
import { prefixReference, routeWithWholeIds, type TextSource } from "./text-references";

/**
 * The queries behind a `dispatch://` reference's target, shared by every surface that resolves
 * one: the inline `RefLink` inside a rendered body, the `Unfurl` card under a bare-reference
 * body, and the `RefPreview` hover card. One module, so they all draw from one fetch path and
 * one cache key per record.
 *
 * A reference to an issue or a document reads what it shows through a summary read, never the
 * full issue: an issue and its documents share `issueSummaryQuery`, a project document has
 * `documentSummaryQuery`. A reference to a block of a document reads the block's graph read
 * (`blockQuery`), which carries the block's text. A page can link a hundred of them, so a healthy
 * summary or block read is fetched again only by an event that can change it (`staleTime:
 * Infinity`); the event refresh rules name these keys. Only a read in error is fetched again on
 * window focus (`refetchFailedOnFocus`). An ask, comment or message reference reads its own
 * record alone.
 */

/**
 * Window focus fetches a summary or block read again only while it is in error, so a read that
 * failed (a 503 past its retries) recovers on the open page while a healthy one stays quiet. It
 * is fetched even over the data it holds, which an event's refetch that failed leaves fresh by
 * age (`staleTime: Infinity`).
 */
const refetchFailedOnFocus = (query: { readonly state: { readonly status: QueryStatus } }) =>
  query.state.status === "error" ? "always" : false;

export { refetchFailedOnFocus };

export interface ReferenceTarget {
  readonly title: string | undefined;
  /** An issue's status, for a reference to the issue itself - its own address or a tab of its
   *  page (`issueOfRoute`) - from the same summary read its title comes from
   *  (`issueSummaryQuery`), which an event moving the title or the status reads again: what its
   *  status mark draws. Undefined while loading and for every other target, a document of the
   *  issue among them. */
  readonly status: string | undefined;
  /** What the target says, as its author wrote it (Markdown): a message's body, a document's
   *  text. `undefined` while loading or for a target with nothing to say. */
  readonly description: string | undefined;
  /** The reading Go made of `description`, which a card draws (`MarkdownPreview`); absent for a
   *  description Dispatch does not store, a GitHub body among them. */
  readonly descriptionTree: DisplayTree | undefined;
  /** The bytes route of the picture a reference to an image artifact names - the version it pins,
   *  else the latest - for a thumbnail beside the title; undefined for anything else. */
  readonly picture: string | undefined;
  /** Whether Dispatch answered that the target does not exist (`ReferenceData.missing`): such a
   *  reference reads as the words its writer wrote, with no link (R8). */
  readonly missing: boolean;
  /** Whether Dispatch answered that a target with no title of its own exists: a session it holds
   *  a record of that the registry does not name. A comment links such a reference in the words
   *  its writer wrote, and the document editor draws it the same way (`document-references.tsx`),
   *  so the two surfaces give one answer. False for every other target, whose title says it. */
  readonly known: boolean;
}

/** What a target shows of itself, which each kind reads its own way; `missing`, `known` and
 *  `status` are answered once for every kind (`useReferenceTarget`). */
type ShownTarget = Omit<ReferenceTarget, "missing" | "known" | "status">;

/** How much of a target's own text becomes a reference link's title: an ask's question, a
 *  comment's body. Long enough to recognise, short enough for a link inside a sentence. */
const TITLE_HEADLINE_MAX = 60;

/** How many characters a link to `route` titles its target in (`useReferenceTarget`): a target
 *  a link titles by its own text - an ask, a comment, a message and a block - in the first
 *  `TITLE_HEADLINE_MAX` of it and an ellipsis, as `useMarkdownHeadline` cuts a headline, and any
 *  other - an issue, a document, a component, a party - by its whole title. A surface that names
 *  a target from another read (`reference-names.tsx`) cuts its name by this, so a name reads as
 *  a link to its target reads it (R8). */
export function linkTitleMax(route: DispatchReferenceRoute | undefined): number {
  switch (route === undefined ? undefined : referenceTargetKind(route)) {
    case "ask":
    case "block":
    case "comment":
    case "message":
      return TITLE_HEADLINE_MAX;
    default:
      return Number.POSITIVE_INFINITY;
  }
}

/** How a title reads the references in the text it is cut from: it reads none of them, so a
 *  `dispatch://` one reads as its short form for good (`ReferenceNaming.readsNone`), never as a
 *  name. R8 is the rule for a stored record drawn on a surface; a title cut from a target's own
 *  text is that target's name, and inside it an address reads as its short form. A name inside
 *  a name resolves nothing, would put another node's whole title inside this one's, and would
 *  change the title's words when a background read lands. One shared value, so the projection
 *  keeps one identity across renders. */
const TITLE_NAMING: ReferenceNaming = { names: new Map(), readsNone: true };

/** Whether a read stands on anything but Dispatch's answer that its target does not exist: what
 *  a mount asks again (`REFERENCE_READ`). */
function notAnsweredMissing(query: { readonly state: { readonly error: unknown } }): boolean {
  return !answeredNotFound(query.state.error);
}

/**
 * What every read a drawn reference observes is held to when its read failed, which is two
 * cases. A target Dispatch answered
 * does not exist (a 404) is an answer: the reference is drawn as the words its writer wrote by
 * design (R8), and the read is not asked again because another reference to the same target
 * mounts, or the same one mounts again. TanStack's default asks such a read again on every
 * mount, each time with the tries `queryRetry` allows - by `retryOnMount` where the read holds
 * no data, and by `refetchOnMount` where it does, since a failed fetch marks the data it keeps
 * stale - so a page naming one missing target in several places asked for it over and over.
 * A failure that can pass - a 5xx, a dropped connection - is asked again when a reference to
 * that target next mounts, within the retry rule, so a target the page could not read a moment
 * ago comes to be named. A read that stands answered is held as it is: it is never stale by age
 * (`staleTime`), and only an event's invalidation makes it so. An invalidation still asks
 * either again, and so does a focus where the read asks for that (`refetchFailedOnFocus`, and
 * TanStack's default for every other read). It is an observer's option, never a query's: a page
 * of its own that observes the same key - an ask's, a document's - keeps TanStack's default.
 */
export const REFERENCE_READ = {
  refetchOnMount: notAnsweredMissing,
  retryOnMount: notAnsweredMissing,
} as const;

/**
 * One read a drawn reference makes, or none: `options` while the reference has what the read
 * needs, undefined until then. TanStack reads `retryOnMount` only where an observer subscribes
 * with its read enabled; an observer whose read is enabled later, or whose key changes - a
 * reference whose short id resolves after it mounts, a document whose id arrives with its
 * issue, a question map once a `#id` is typed - asks a failed read again whatever
 * `REFERENCE_READ` says (`shouldFetchOptionally`). So the read is a list of zero or one read
 * (`useQueries`): a read that arrives later is a new observer, which subscribes enabled and
 * holds to `REFERENCE_READ` as a read made at mount does. A read that says otherwise for itself
 * keeps what it says - the registry, which the shell's poller refreshes, is never read again on
 * mount. Undefined while there is no read.
 */
export function useReferenceQuery<
  TQueryFnData,
  TData = TQueryFnData,
  TQueryKey extends QueryKey = QueryKey,
>(
  options: UseQueryOptions<TQueryFnData, Error, TData, TQueryKey> | undefined
): UseQueryResult<TData, Error> | undefined {
  const queries: UseQueryOptions<TQueryFnData, Error, TData, TQueryKey>[] =
    options === undefined ? [] : [{ ...REFERENCE_READ, ...options }];
  const [result] = useQueries({ queries });
  return result as UseQueryResult<TData, Error> | undefined;
}

/** What a reference to an issue, or to one of its documents, shows: the issue's title and status
 *  and each document's name, kind, latest version and whether it is the spec
 *  (`GET /issues/{key}/summary`). Fetched again only by an event that can change it, or on focus
 *  after it failed. */
export const issueSummaryQuery = (key: string | undefined) =>
  queryOptions({
    queryKey: ["issue-summary", key],
    queryFn: () => api.getIssueSummary(key ?? ""),
    staleTime: Number.POSITIVE_INFINITY,
    refetchOnWindowFocus: refetchFailedOnFocus,
  });

/** What a reference to a project document shows: its name, kind and latest version
 *  (`GET /projects/{key}/artifacts/{slug}/summary`). Fetched again only by an event that can
 *  change it, or on focus after it failed. */
export const documentSummaryQuery = (project: string | undefined, slug: string | undefined) =>
  queryOptions({
    queryKey: ["doc-summary", project, slug],
    queryFn: () => api.getProjectDocumentSummary(project ?? "", slug ?? ""),
    staleTime: Number.POSITIVE_INFINITY,
    refetchOnWindowFocus: refetchFailedOnFocus,
  });

/** One issue message and its replies, as the Conversation's landing, a reference and the hover
 *  card read it. Outside the `["issue"]` prefix, so a refresh of issue details leaves it alone. */
export const messageQuery = (key: string | undefined, id: string | undefined) =>
  queryOptions({
    queryKey: ["issue-message", key, id],
    queryFn: () => api.getMessage(key ?? "", id ?? ""),
  });

/** An artifact of an agent's conversation, by its session and slug. */
export const agentArtifactQuery = (session: string | undefined, slug: string | undefined) =>
  queryOptions({
    queryKey: ["agent-artifacts", session, slug],
    queryFn: () => api.getAgentArtifact(session ?? "", slug ?? ""),
  });

/** Every artifact of an agent's conversation, newest first: what the picker opens under the
 *  session's row. Its key is the prefix of each one's own read above, so an upload to the
 *  conversation (`refreshOwnerArtifacts`) refreshes the list and its members alike. */
export const agentArtifactsQuery = (session: string | undefined) =>
  queryOptions({
    queryKey: ["agent-artifacts", session],
    queryFn: () => api.listAgentArtifacts(session ?? ""),
  });

/** A document's live text, or one immutable version of it when the reference pins a version. */
export const artifactTextQuery = (id: string | undefined, version: number | undefined) =>
  queryOptions<ArtifactText | ArtifactVersionContent>({
    queryKey: ["artifact", id, version ?? "text"],
    queryFn: () =>
      version === undefined
        ? api.getArtifactText(id ?? "")
        : api.getArtifactVersion(id ?? "", version),
  });

/** One ask with its thread, followers, and edits. */
export const askQuery = (id: string | undefined) =>
  queryOptions({
    queryKey: ["ask", id],
    queryFn: () => api.getAsk(id ?? ""),
  });

/** One comment with its replies. */
export const commentQuery = (id: string | undefined) =>
  queryOptions({
    queryKey: ["comment", id],
    queryFn: () => api.getComment(id ?? ""),
  });

/** A project's architecture, which is where a component's own title is: the one read behind a
 *  component reference, keyed as the picker's own read of the same tree is
 *  (`features/refs/useRefPicker.ts`), so a box that has listed a project's components and a
 *  body that names one of them share the answer. */
export const architectureQuery = (project: string | undefined) =>
  queryOptions({
    queryKey: ["architecture", project],
    queryFn: () => api.getArchitecture(project ?? ""),
  });

/** `GET /api/v1/references?to=<reference>`: the node a reference names and the edges pointing at
 *  it, read by the Referenced by panel and, for a block, by the reference's title and card. */
export const referencesQuery = (reference: string | undefined) =>
  queryOptions({
    queryKey: ["references", "to", reference] as const,
    queryFn: () => api.getReferences(reference ?? ""),
  });

/** A block reference's graph read, under the Referenced by panel's key: its node carries the
 *  block's whole text and, for a pinned reference, its pin. A link to a block reads it as a link
 *  to an issue reads its summary, fetched again only by an event that can change it (a version of
 *  the block's document, a citation of the block added or removed, an edit of the comment whose
 *  block it is, or a reply that names the block), or on focus after it failed. A read that answers
 *  that the block is gone shows none of the text it held before (`useReferenceData`). */
export const blockQuery = (reference: string | undefined) =>
  queryOptions({
    ...referencesQuery(reference),
    staleTime: Number.POSITIVE_INFINITY,
    refetchOnWindowFocus: refetchFailedOnFocus,
  });

/** `GET /api/v1/references/questions?kind=&id=`: the question map a `#id` in one source's text
 *  resolves against, which is the server's own map (`refs.ScopeOf` for the document that text
 *  belongs to, `refs.QuestionMap` for the questions that document's latest version declares) -
 *  the one map the page and the reference graph share. Keyed by the source, so two texts of one
 *  source read it once. A source whose text belongs to no document answers 404, which leaves
 *  its `#id`s as plain text, so the read is not retried. */
export const questionsQuery = (source: TextSource | undefined) =>
  queryOptions({
    queryKey: ["references", "questions", source?.kind, source?.id] as const,
    queryFn: async () => {
      // `useTextReferences` enables the read only for a text that names its source, so an
      // absent one never reaches Dispatch.
      if (source === undefined) {
        throw new Error("the question map is read for one named source");
      }
      return await api.getReferenceQuestions(source.kind, source.id);
    },
    retry: false,
  });

/** The question map of a read's answer: each question block id its document declares to the
 *  `dispatch://` reference of the ask settled for it, as the server writes that address. A
 *  block whose ask has no address - an ask on an agent conversation's document - is left out,
 *  since no reference names it. Undefined until the read answers. */
export function questionReferences(
  answer: ReferenceQuestions | undefined
): Readonly<Record<string, string>> | undefined {
  if (answer === undefined) {
    return undefined;
  }
  const questions: Record<string, string> = {};
  for (const question of answer.questions ?? []) {
    if (question.reference !== "") {
      questions[question.block_id] = question.reference;
    }
  }
  return questions;
}

/** The document a reference names, as much of it as a reference shows: an issue summary's
 *  document, a project document's summary, or an agent conversation's artifact cut to the same
 *  fields. `latest_version` is its greatest version number, null when it has none. */
export type ReferenceArtifact = Pick<
  IssueReferenceSummaryDocument,
  "id" | "kind" | "latest_version" | "name"
>;

/** An agent conversation's artifact as a `ReferenceArtifact`; its versions are in number order. */
function agentReferenceArtifact(artifact: Artifact): ReferenceArtifact {
  return {
    id: artifact.id,
    kind: artifact.kind,
    latest_version: (artifact.versions ?? []).at(-1)?.number ?? null,
    name: artifact.name,
  };
}

/** The issue's spec among an issue summary's documents, or undefined until the summary is read. */
export function summarySpec(
  summary: IssueReferenceSummary | undefined
): IssueReferenceSummaryDocument | undefined {
  return (summary?.documents ?? []).find((document) => document.primary);
}

/** The block a route names, as the reference its graph read is keyed by; undefined for a route
 *  that names no block. */
function blockReferenceOf(route: DispatchReferenceRoute | undefined): string | undefined {
  return route !== undefined && "block" in route && route.block !== undefined
    ? buildDispatchReference(route)
    : undefined;
}

/** The reference's target records, each present once its query resolved. `issue` is the issue
 * summary behind an issue or issue-document reference; `artifact` is the document a
 * document/artifact reference names (never the owning issue's primary spec); `markdown` is that
 * document's text at the referenced version, read only when the caller asked for it; `block` is a
 * block reference's graph read, whose node carries the block's whole text and, for a pinned
 * reference, its pin, undefined once the read answers that the block is gone. A block reference
 * reads that alone: its document's name is the hover card's own read (`useBlockDocument`). */
export interface ReferenceData {
  readonly issue: IssueReferenceSummary | undefined;
  readonly artifact: ReferenceArtifact | undefined;
  readonly markdown: string | undefined;
  /** The reading Go made of `markdown`, which a card draws. */
  readonly markdownTree: DisplayTree | undefined;
  readonly ask: AskRead | undefined;
  readonly comment: CommentRead | undefined;
  readonly message: MessageRead | undefined;
  readonly block: GraphReferences | undefined;
  /** The component a component reference names, once its project's architecture is read: the
   *  one record behind that kind, and what gives the reference the component's own title. */
  readonly component: ArchitectureComponentRead | undefined;
  /** A session reference's title: the registry's, trimmed, for a session it lists with a
   *  non-blank one (`registeredTitle`), else the one Dispatch answered for a session it holds a
   *  record of (`known-sessions.ts`); undefined otherwise. */
  readonly sessionTitle: string | undefined;
  /** Whether Dispatch holds a record of the session a session reference names. */
  readonly sessionKnown: boolean;
  /** Whether Dispatch knows the person a person reference names: their login is one of the
   *  people who have signed in, the people the reference picker lists where no issue is in hand
   *  (`useRefPicker.ts`). False while that list loads and for any other reference. */
  readonly personKnown: boolean;
  /** Whether the role a role reference names is known (`role-holders.ts`): a session the
   *  registry lists holds it, or the collaborators of the issue the record is on record it - the
   *  roles the picker offers, running and not (`party-options.ts`). False while those load and
   *  for any other reference. */
  readonly roleKnown: boolean;
  /** Whether Dispatch answered that the target does not exist: the read of the record behind the
   *  reference answered 404 (a project with no architecture among them), or the read that lists
   *  it came back without it - an issue's artifacts with neither the slug nor, for a document, the
   *  name an artifact reference writes (`artifactAtAddress`), a project's architecture without the
   *  component, Dispatch's record of sessions without the session (one the registry lists, and
   *  one that ended, are both known), the registry without a session that holds the role and the
   *  record's issue's collaborators without it. False while loading and for any other failure, so
   *  a reference that names a real target never loses its link to a slow or dropped read. The
   *  document editor reads the same answer (`document-references.tsx`, through
   *  `useReferenceTarget`), so a comment and the editor cannot disagree. */
  readonly missing: boolean;
}

export interface ReferenceReadOptions {
  /** Whether to read the named document's text into `markdown`. The Unfurl card and the hover
   *  card show it; an inline link shows only a title and reads none. */
  readonly text: boolean;
}

/** Warms every query the hover card will read for route, so a card mounted after the hover
 * delay renders populated instead of in its loading form: an issue's full read and its primary
 * spec text (the card's issue view, which no link reads); a document's summary, then its text once
 * its id is known; a block's graph read and, for a block of a document, the summary that names its
 * document, and no document text (a block of a comment or message is its graph read alone, whose
 * node carries the block's text); an ask's, comment's or message's own read. Every key comes from
 * the query builders above. */
export function prefetchReference(queryClient: QueryClient, route: DispatchReferenceRoute): void {
  const warmText = (
    artifact: Pick<ReferenceArtifact, "id" | "kind"> | undefined,
    version: number | undefined
  ) => {
    if (artifact?.kind === "doc") {
      void queryClient.prefetchQuery(artifactTextQuery(artifact.id, version));
    }
  };
  const block = blockReferenceOf(route);
  if (block !== undefined) {
    void queryClient.prefetchQuery(blockQuery(block));
    if (itemOfBlock(route) !== undefined) {
      return;
    }
    if (route.kind === "document") {
      void queryClient.prefetchQuery(documentSummaryQuery(route.project, route.slug));
    } else if ("key" in route) {
      void queryClient.prefetchQuery(issueSummaryQuery(route.key));
    }
    return;
  }
  switch (route.kind) {
    case "agent-artifact": {
      const query = agentArtifactQuery(route.session, route.slug);
      void queryClient.prefetchQuery(query).then(() => {
        const artifact = queryClient.getQueryData(query.queryKey);
        warmText(
          artifact === undefined ? undefined : agentReferenceArtifact(artifact),
          route.version
        );
      });
      return;
    }
    case "message":
      void queryClient.prefetchQuery(messageQuery(route.key, route.id));
      return;
    case "ask":
      void queryClient.prefetchQuery(askQuery(route.id));
      return;
    case "comment":
      void queryClient.prefetchQuery(commentQuery(route.id));
      return;
    case "document": {
      if (route.item?.kind === "ask") {
        void queryClient.prefetchQuery(askQuery(route.item.id));
        return;
      }
      if (route.item?.kind === "comment") {
        void queryClient.prefetchQuery(commentQuery(route.item.id));
        return;
      }
      const query = documentSummaryQuery(route.project, route.slug);
      void queryClient
        .prefetchQuery(query)
        .then(() => warmText(queryClient.getQueryData(query.queryKey), route.version));
      return;
    }
    case "artifact": {
      const query = issueSummaryQuery(route.key);
      void queryClient.prefetchQuery(query).then(() => {
        warmText(
          artifactAtAddress(queryClient.getQueryData(query.queryKey)?.documents, route.slug),
          route.version
        );
      });
      return;
    }
    case "agent-session":
    case "role":
    case "person":
      // A party is a reader, not a record: there is no read behind one to warm.
      return;
    case "component":
      void queryClient.prefetchQuery(architectureQuery(route.project));
      return;
    default: {
      const query = issueQuery(route.key);
      void queryClient
        .prefetchQuery(query)
        .then(() => warmText(primarySpec(queryClient.getQueryData(query.queryKey)), undefined));
    }
  }
}

/** The whole id each short id of a prefix's graph read names, to the reference it is stored
 *  under: a block by its block id, every other record by its own. A read that answered nothing
 *  names nothing, and the reference stays as the text wrote it. */
export function resolvedPrefixItems(
  answer: GraphReferences | undefined
): Readonly<Record<string, string>> {
  const node = answer?.node;
  const reference = node?.ref;
  if (node === undefined || reference === undefined) {
    return {};
  }
  const id = node.block_id ?? node.id;
  return id === "" ? {} : { [id]: reference };
}

/** `useWholeRouteRead`'s answer: the route with its short ids replaced, the reference its graph
 *  read was keyed by (undefined for a route that writes every id whole, which reads nothing),
 *  and that read, for a caller that also shows what an ambiguous prefix matched. */
export interface WholeRouteRead {
  readonly prefix: string | undefined;
  readonly resolution: UseQueryResult<GraphReferences>;
  readonly route: DispatchReferenceRoute | undefined;
}

/**
 * `route` with every id it writes short replaced by the whole id it names, which one graph read
 * of the reference as written answers (`GET /api/v1/references?to=…`, which the server resolves
 * within the owner the reference names). A route that writes every id whole - every reference a
 * page or a node gave - is answered as it stands and reads nothing extra. One whose short id
 * names nothing yet, or nothing at all, is `undefined`, so the record behind it is read under
 * its whole id or not at all and nothing is ever cached under a prefix.
 *
 * The read itself is returned beside it, for the surfaces that show an ambiguous prefix's
 * matches (`ArtifactDocument`, `useItemLanding`); a caller that wants the route alone reads
 * `.route`. `observer` is the caller's own options for that read: a drawn reference passes
 * `REFERENCE_READ`, and a page of its own leaves TanStack's defaults.
 */
export function useWholeRouteRead(
  route: DispatchReferenceRoute | undefined,
  observer: Partial<typeof REFERENCE_READ> = {}
): WholeRouteRead {
  const origin = window.location.origin;
  const prefix = route === undefined ? undefined : prefixReference(route);
  const resolution = useQuery({
    ...referencesQuery(prefix),
    ...observer,
    enabled: prefix !== undefined,
  });
  return {
    prefix,
    resolution,
    route:
      route === undefined || prefix === undefined
        ? route
        : routeWithWholeIds(route, resolvedPrefixItems(resolution.data), origin),
  };
}

/** Whether a read failed because Dispatch answered that its record does not exist. */
function answeredNotFound(error: unknown): boolean {
  return error instanceof ApiError && error.status === 404;
}

/** Every read behind one drawn reference, each made through `useReferenceQuery`, since each
 *  waits on something the reference may not have at mount: a short id's whole id, a document's
 *  id, the registry's answer. */
export function useReferenceData(
  given: DispatchReferenceRoute | undefined,
  options: ReferenceReadOptions
): ReferenceData {
  const route = useWholeRouteRead(given, REFERENCE_READ).route;
  const blockReference = blockReferenceOf(route);
  // A block of a comment or message is read from its graph node alone, which carries its text:
  // no issue, document, ask, comment or message read behind it.
  const record = route !== undefined && itemOfBlock(route) !== undefined ? undefined : route;
  const issueRoute =
    route === undefined || !("key" in route) || blockReference !== undefined ? undefined : route;
  // An ask, comment or message under an issue shows only its own record.
  const summaryKey =
    issueRoute === undefined ||
    issueRoute.kind === "ask" ||
    issueRoute.kind === "comment" ||
    issueRoute.kind === "message"
      ? undefined
      : issueRoute.key;
  // An ask or comment on a project document shows only its own record, and a block its own text.
  const document =
    route?.kind === "document" && route.item === undefined && blockReference === undefined
      ? route
      : undefined;
  const agentArtifact = route?.kind === "agent-artifact" ? route : undefined;
  const message = record?.kind === "message" ? record : undefined;
  const version =
    route?.kind === "artifact" || route?.kind === "document" || route?.kind === "agent-artifact"
      ? route.version
      : undefined;
  const askId =
    record?.kind === "ask"
      ? record.id
      : record?.kind === "document" && record.item?.kind === "ask"
        ? record.item.id
        : undefined;
  const commentId =
    record?.kind === "comment"
      ? record.id
      : record?.kind === "document" && record.item?.kind === "comment"
        ? record.item.id
        : undefined;
  const issue = useReferenceQuery(
    summaryKey === undefined ? undefined : issueSummaryQuery(summaryKey)
  );
  const messageQueryResult = useReferenceQuery(
    message === undefined ? undefined : messageQuery(message.key, message.id)
  );
  const projectDocument = useReferenceQuery(
    document === undefined ? undefined : documentSummaryQuery(document.project, document.slug)
  );
  const sessionArtifact = useReferenceQuery(
    agentArtifact === undefined
      ? undefined
      : {
          ...agentArtifactQuery(agentArtifact.session, agentArtifact.slug),
          select: agentReferenceArtifact,
        }
  );
  const block = useReferenceQuery(
    blockReference === undefined ? undefined : blockQuery(blockReference)
  );
  const artifact: ReferenceArtifact | undefined =
    document !== undefined
      ? projectDocument?.data
      : agentArtifact !== undefined
        ? sessionArtifact?.data
        : route?.kind === "artifact"
          ? artifactAtAddress(issue?.data?.documents, route.slug)
          : undefined;
  const text = useReferenceQuery(
    options.text && artifact?.kind === "doc" ? artifactTextQuery(artifact.id, version) : undefined
  );
  const ask = useReferenceQuery(askId === undefined ? undefined : askQuery(askId));
  const comment = useReferenceQuery(commentId === undefined ? undefined : commentQuery(commentId));
  const componentRoute = route?.kind === "component" ? route : undefined;
  const architecture = useReferenceQuery(
    componentRoute === undefined ? undefined : architectureQuery(componentRoute.project)
  );
  const component = (architecture?.data?.components ?? []).find(
    (candidate) => candidate.id === componentRoute?.id
  );
  // A session is a target while Dispatch holds any record of it: listed in the registry, which
  // the page already reads, or, for one the registry does not list, answered by the one batched
  // read of the page's unlisted sessions. An ended session keeps its conversation, so it stays a
  // link; an id no agent ever held does not (R8). While a session reference is drawn it keeps the
  // shell's one registry poll running, so a session renamed since the page loaded reads as it is
  // called now (R4) - in a draft's pill as in the record. A role reference reads the same
  // registry, by the one rule of what a known role is (`role-holders.ts`), which the picker's
  // role rows follow too, and keeps the same poll running, so a role a session takes up while
  // the page is open comes to be named.
  const sessionRoute = route?.kind === "agent-session" ? route : undefined;
  const roleRoute = route?.kind === "role" ? route : undefined;
  const readsRegistry = sessionRoute !== undefined || roleRoute !== undefined;
  useRegistryPolling(readsRegistry);
  const registry = useReferenceQuery(
    readsRegistry ? { ...agentsQuery(), refetchOnMount: false } : undefined
  );
  const listed =
    sessionRoute === undefined ? undefined : registeredAgent(registry?.data, sessionRoute.session);
  // The registry answered and does not list the session: only then is Dispatch's record of it
  // read, and only then can that record's answer make the reference missing.
  const unlisted =
    sessionRoute !== undefined && registry?.data !== undefined && listed === undefined;
  const recorded = useReferenceQuery(
    unlisted ? knownSessionQuery(sessionRoute.session) : undefined
  );
  const listedTitle =
    sessionRoute === undefined ? undefined : registeredTitle(registry?.data, sessionRoute.session);
  // A role is known while a session the registry lists holds it, or while the collaborators of
  // the issue the record is on record it: the issue's stopped roles, which the picker offers
  // under "Not running". Those collaborators are read only for a role the registry answered no
  // holder for, on a record of an issue, from the one entry the picker and the Collaborators tab
  // read. A role neither answer knows is no target: it reads as the words its writer wrote,
  // with no link. A collaborators answer made while the Legion registry did not answer holds no
  // role of anyone, so it is no answer about this one.
  const recordIssue = useContext(RecordIssue);
  const roleHeld =
    roleRoute !== undefined && roleHolders(registry?.data ?? EMPTY_AGENTS).has(roleRoute.role);
  const roleUnheld = roleRoute !== undefined && registry?.isSuccess === true && !roleHeld;
  const collaborators = useReferenceQuery(
    roleUnheld && recordIssue !== undefined ? issueCollaboratorsQuery(recordIssue) : undefined
  );
  const roleKnown =
    roleHeld ||
    (roleRoute !== undefined && recordedRoles(collaborators?.data?.agents).has(roleRoute.role));
  const roleUnknown =
    roleUnheld &&
    !roleKnown &&
    (collaborators === undefined ||
      collaborators.data?.legion_registry === "ok" ||
      answeredNotFound(collaborators.error));
  // A person is named by their login, which their address holds, once Dispatch knows the login:
  // the one read of everyone who has signed in, shared with every other person reference and
  // with the pickers that list them, and made only for a person reference.
  const personRoute = route?.kind === "person" ? route : undefined;
  const people = useReferenceQuery(personRoute === undefined ? undefined : usersQuery());
  const personKnown =
    personRoute !== undefined &&
    (people?.data ?? []).some((person) => person.login === personRoute.login);
  const textRead = text?.data;
  return {
    issue: issue?.data,
    artifact,
    markdown: textRead !== undefined && "markdown" in textRead ? textRead.markdown : undefined,
    markdownTree:
      textRead !== undefined && "markdown_tree" in textRead ? textRead.markdown_tree : undefined,
    ask: ask?.data,
    comment: comment?.data,
    message: messageQueryResult?.data,
    component,
    sessionTitle: listedTitle ?? recorded?.data?.title,
    sessionKnown: listed !== undefined || recorded?.data?.known === true,
    personKnown,
    roleKnown,
    missing:
      (unlisted && recorded?.data?.known === false) ||
      roleUnknown ||
      [
        issue,
        messageQueryResult,
        projectDocument,
        sessionArtifact,
        block,
        ask,
        comment,
        architecture,
      ].some((read) => answeredNotFound(read?.error)) ||
      (componentRoute !== undefined &&
        architecture?.data !== undefined &&
        component === undefined) ||
      (route?.kind === "artifact" && issue?.data !== undefined && artifact === undefined),
    // TanStack keeps a query's last data through a failed refetch, so a link or card open when an
    // edit deletes its block would go on showing text its document or item no longer holds. The
    // read answers 404 `NOT_FOUND` for that block, and the link and card then read as a freshly
    // loaded page reads the same link. A pinned reference to a removed block answers 200 with the
    // pin's `removed` state and keeps its pinned text; any other failure (a dropped connection, a
    // 5xx) leaves the last data shown.
    block:
      block?.error instanceof ApiError &&
      block.error.status === 404 &&
      block.error.code === "NOT_FOUND"
        ? undefined
        : block?.data,
  };
}

/** The document a block reference names, as the hover card shows it: the spec or the issue
 *  document from the issue's summary, or the project document's summary. Undefined until read, and
 *  for a route that names no block of a document: a block of a comment or message names its item,
 *  never a document, so it reads no summary. */
export function useBlockDocument(
  route: DispatchReferenceRoute | undefined
): ReferenceArtifact | undefined {
  const named =
    route === undefined || blockReferenceOf(route) === undefined || itemOfBlock(route) !== undefined
      ? undefined
      : route;
  const project = named?.kind === "document" ? named : undefined;
  const issueKey =
    named !== undefined && named.kind !== "document" && "key" in named ? named.key : undefined;
  const issue = useQuery({
    ...issueSummaryQuery(issueKey),
    ...REFERENCE_READ,
    enabled: issueKey !== undefined,
  });
  const projectDocument = useQuery({
    ...documentSummaryQuery(project?.project, project?.slug),
    ...REFERENCE_READ,
    enabled: project !== undefined,
  });
  switch (named?.kind) {
    case "document":
      return projectDocument.data;
    case "spec":
      return summarySpec(issue.data);
    case "artifact":
      return artifactAtAddress(issue.data?.documents, named.slug);
    default:
      return undefined;
  }
}

/** The text a block reference shows: the text the pinned version held for a pinned reference, the
 *  block's current text otherwise. */
export function blockText(node: GraphReferences["node"]): string | undefined {
  return node.pin === undefined ? node.text : node.pin.text;
}

/** The reading Go made of the text `blockText` answers, which a reference's title and card draw
 *  rather than parsing the block's markdown again. */
export function blockTextTree(node: GraphReferences["node"]): DisplayTree | undefined {
  return node.pin === undefined ? node.text_tree : node.pin.text_tree;
}

/**
 * Every query behind a resolved reference title, shared by the Unfurl card and the inline
 * `RefLink` markdown/document rendering so both draw from one fetch path. An ask, a comment or
 * an issue message (whether issue-scoped or nested under a project document's `item`) resolves
 * to its own text rather than the owning issue's title or document; everything else falls back
 * to the artifact name (a document reference) or the issue title. A title drawn from text its
 * author wrote (the question, the comment, the message's body) is that Markdown projected to
 * plain words and cut (`useMarkdownHeadline`): a link's text can hold no formatting, so a
 * `**Blocking:**` question titles its link `Blocking: …`, never `**Blocking:** …`. The
 * description is the target's own
 * Markdown, for the caller to render formatted: a message's body, a document's text (read only
 * with `options.text`), and an ask's or comment's text where the title had to cut it (a title
 * that holds the whole text needs no second copy under it); an issue with no document falls back
 * to its status. An ask or comment never borrows its issue's status or its document's text, while
 * it loads or after. A block of a document resolves to the headline of its own text, as an ask or
 * comment does: the text the pinned version held for a pinned reference, its current text
 * otherwise. A component resolves to its own title from its project's architecture, the one
 * record behind that kind. A session resolves to its title, a role to its token while it is
 * known (a session the registry lists holds it, or the record's issue's collaborators record it),
 * and a person to their login once Dispatch knows it (R5). A reference to an image artifact, of
 * an issue, a project or an agent's conversation, also answers its picture: the bytes route of
 * the version it pins, else of the latest.
 */
export function useReferenceTarget(
  route: DispatchReferenceRoute | undefined,
  options: ReferenceReadOptions
): ReferenceTarget {
  const {
    artifact,
    ask,
    block,
    comment,
    component,
    issue,
    markdown,
    markdownTree,
    message,
    missing,
    personKnown,
    roleKnown,
    sessionKnown,
    sessionTitle,
  } = useReferenceData(route, options);
  const kind = route === undefined ? undefined : referenceTargetKind(route);
  let authored: string | undefined;
  let authoredTree: DisplayTree | undefined;
  switch (kind) {
    case "ask":
      authored = ask?.ask.question;
      authoredTree = ask?.ask.question_tree;
      break;
    case "comment":
      authored = comment?.comment.body;
      authoredTree = comment?.comment.body_tree;
      break;
    case "block":
      authored = block === undefined ? undefined : blockText(block.node);
      authoredTree = block === undefined ? undefined : blockTextTree(block.node);
      break;
    case "message":
      authored = message?.message.body;
      authoredTree = message?.message.body_tree;
      break;
  }
  // The tree is the reading Go made of this text, and where there is one nothing here parses
  // markdown. An answer that carries none still has a title: a server that has not rolled yet,
  // or any caller that did not ask for trees, answers the markdown alone, and a deploy serving
  // two builds has a dashboard reading both at once. `useMarkdownHeadline` reads the fallback
  // only where `tree` is undefined, so a stored text that came with its tree is still parsed
  // nowhere (`e2e/parse-count.e2e.ts`). The title reads none of the references in it, so each
  // `dispatch://` one reads as its short form with either reading (`TITLE_NAMING`).
  const headline = useMarkdownHeadline(authoredTree, linkTitleMax(route), authored, TITLE_NAMING);

  let shown: ShownTarget;
  if (kind === "message") {
    // A message reads as the head of what it says, the way a comment does; its own body is the
    // description, which the cards under a message reference render formatted.
    shown = {
      title: headline?.text,
      description: message?.message.body,
      descriptionTree: message?.message.body_tree,
      picture: undefined,
    };
  } else if (kind === "session") {
    // A session reads as its title, and links while Dispatch holds a record of it.
    shown = {
      title: sessionTitle,
      description: undefined,
      descriptionTree: undefined,
      picture: undefined,
    };
  } else if (kind === "component") {
    // A component is named by its architecture row, and says what it is in its own prose, which
    // the architecture read answers with Go's reading of it (`prose_tree`), as the component's
    // own page draws it (`ComponentDetails`).
    shown = {
      title: component?.title,
      description: component?.prose,
      descriptionTree: component?.prose_tree,
      picture: undefined,
    };
  } else if (kind === "ask" || kind === "comment" || kind === "block") {
    shown = {
      title: headline?.text,
      description: headline?.cut === true ? authored : undefined,
      descriptionTree: headline?.cut === true ? authoredTree : undefined,
      picture: undefined,
    };
  } else if (route?.kind === "role") {
    // A role is named by its token, which its address holds, while it is known (R5): a session
    // the registry lists holds it, or the collaborators of the record's issue record it. The page
    // it opens is the role's own. A role neither knows has no name, and reads as the words its
    // writer wrote.
    shown = {
      title: roleKnown ? route.role : undefined,
      description: undefined,
      descriptionTree: undefined,
      picture: undefined,
    };
  } else if (route?.kind === "person") {
    // A person is named by their login, once Dispatch knows it (R5); a login it does not know
    // has no name, and reads as the words its writer wrote.
    shown = {
      title: personKnown ? route.login : undefined,
      description: undefined,
      descriptionTree: undefined,
      picture: undefined,
    };
  } else {
    const text = markdown !== undefined && markdown.trim() !== "" ? markdown : undefined;
    const named = route === undefined ? undefined : referencedArtifact(route);
    // A reference that names a document is titled by that document alone: one its issue does not
    // hold has no title, never its issue's, which would name a node the reference does not name.
    shown = {
      title: named === undefined ? issue?.title : artifact?.name,
      description: text ?? (artifact === undefined ? issue?.status : undefined),
      descriptionTree: text === undefined ? undefined : markdownTree,
      picture: named === undefined ? undefined : picturePath(named, artifact),
    };
  }
  // Only a session can exist with no title of its own: every other target's title says it does.
  // Only the issue itself, by its own address or a tab of its page, has a status of its own.
  return {
    ...shown,
    missing,
    known: kind === "session" && sessionKnown && sessionTitle === undefined,
    status: route !== undefined && issueOfRoute(route) !== undefined ? issue?.status : undefined,
  };
}

/** The bytes route of the picture `named` shows: the version it pins, else `artifact`'s latest.
 *  Undefined unless `artifact` (the document the reference names, once read) is an image with a
 *  version, since a document or another file is no picture the browser can draw. */
function picturePath(
  named: ReferencedArtifact,
  artifact: Pick<ReferenceArtifact, "kind" | "latest_version"> | undefined
): string | undefined {
  const version = named.version ?? artifact?.latest_version;
  return artifact?.kind !== "image" || !version
    ? undefined
    : artifactVersionPath(named.owner, named.slug, version);
}

/** Calls `onPicture` with the picture path a summary's data gives, now and whenever the summary
 *  query changes (an event refreshes it, a failed read recovers), until the returned function
 *  unsubscribes. Undefined while the summary is unread or unreadable. The picture is a drawn
 *  reference, so its read is observed as one (`REFERENCE_READ`). */
function observePicturePath<TData, TKey extends QueryKey>(
  queryClient: QueryClient,
  options: QueryObserverOptions<TData, Error, TData, TData, TKey>,
  artifactOf: (data: TData) => ReferenceArtifact | undefined,
  named: ReferencedArtifact,
  onPicture: (path: string | undefined) => void
): () => void {
  const observer = new QueryObserver(queryClient, { ...options, ...REFERENCE_READ });
  const report = (result: QueryObserverResult<TData, Error>) =>
    onPicture(result.data === undefined ? undefined : picturePath(named, artifactOf(result.data)));
  const unsubscribe = observer.subscribe(report);
  report(observer.getCurrentResult());
  return unsubscribe;
}

/** `useReferenceTarget`'s `picture`, for a caller outside React (the document editors' picture
 *  node view): it observes the same summary queries through `queryClient`, so every picture of
 *  one issue shares one summary read with the links and cards on the page, and follows the
 *  summary as an event refreshes it - a new version uploaded, a failed read recovered. Calls
 *  `onPicture` at once and on every change, with undefined for a reference to anything but an
 *  image artifact or while its summary is unread or unreadable; the returned function stops. */
export function watchPicturePath(
  queryClient: QueryClient,
  route: DispatchReferenceRoute,
  onPicture: (path: string | undefined) => void
): () => void {
  const named = referencedArtifact(route);
  if (named !== undefined && route.kind === "document") {
    return observePicturePath(
      queryClient,
      documentSummaryQuery(route.project, route.slug),
      (summary) => summary,
      named,
      onPicture
    );
  }
  if (named !== undefined && route.kind === "agent-artifact") {
    return observePicturePath(
      queryClient,
      agentArtifactQuery(route.session, route.slug),
      agentReferenceArtifact,
      named,
      onPicture
    );
  }
  if (named !== undefined && route.kind === "artifact") {
    return observePicturePath(
      queryClient,
      issueSummaryQuery(route.key),
      (summary) => artifactAtAddress(summary.documents, route.slug),
      named,
      onPicture
    );
  }
  onPicture(undefined);
  return () => {};
}

/** Whether a reference's path waits on its issue: a block of an issue's spec pinned to a version
 *  opens that version under the spec's own slug, which only the issue names. */
export function needsSpecSlug(
  route: DispatchReferenceRoute
): route is Extract<IssueRoute, { kind: "spec" }> & { block: { id: string; pin: string } } {
  return route.kind === "spec" && route.block?.pin !== undefined;
}

/** The `href` a reference opens inside this dashboard (`referenceHref`), or undefined for one
 *  that opens no page of it - a person, whom nothing shows (R5), and any path the gate refuses.
 *  A block of an issue's spec pinned to a version opens that version under the spec's own slug,
 *  so its path reads the issue's summary (the query an issue reference's title and the block's
 *  card already share); until it is read it is the live spec at the block. No other reference's
 *  path reads anything. */
export function useReferenceHref(route: DispatchReferenceRoute | undefined): string | undefined {
  const specKey = route !== undefined && needsSpecSlug(route) ? route.key : undefined;
  const issue = useQuery({
    ...issueSummaryQuery(specKey),
    ...REFERENCE_READ,
    enabled: specKey !== undefined,
  });
  return route === undefined ? undefined : referenceHref(route, summarySpec(issue.data)?.slug);
}

/** `useReferenceHref`'s answer for a click outside a render: it waits for the issue summary a
 *  pinned block of a spec needs, and opens the live spec at the block when the summary cannot be
 *  read, as the hook's href does before it is. */
export async function fetchReferenceHref(
  queryClient: QueryClient,
  route: DispatchReferenceRoute
): Promise<string | undefined> {
  if (!needsSpecSlug(route)) {
    return referenceHref(route);
  }
  const issue = await queryClient.fetchQuery(issueSummaryQuery(route.key)).catch(() => undefined);
  return referenceHref(route, summarySpec(issue)?.slug);
}
