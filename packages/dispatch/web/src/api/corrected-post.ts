import {
  type InfiniteData,
  type Mutation,
  type Query,
  type QueryClient,
  type QueryKey,
  replaceEqualDeep,
} from "@tanstack/react-query";

import { type PostDeletion, postDeletion } from "../features/refs/post-marks";
import { compareTimestamps, isTimestamp } from "../lib/timestamps";

import { ApiError, ambiguousItems } from "./client";
import { correctedPostAddresses, rowsOf } from "./event-refresh";
import type { ReferenceTargetsAnswer } from "./queries";
import { replaceQueryData } from "./query-client";
import type {
  Actor,
  Artifact,
  ArtifactVersionContent,
  AskThread,
  CommentEventPayload,
  CommentRead,
  CommentWithTrees,
  DispatchEvent,
  DisplayTree,
  GraphEdge,
  GraphReferences,
  IDCandidate,
  IssueDetails,
  IssueReferencesAnswer,
  MessageEventPayload,
  MessageRead,
  MessageWithTrees,
  MyAnswerRow,
  MyAnswersResponse,
  MyRepliesResponse,
  MyReplyRow,
  PersonNode,
  PersonNodes,
  ReferenceLink,
  ReferenceTarget,
  ReplyBlockWithTrees,
  SearchResponse,
  SearchResult,
  Version,
} from "./types";

/** What a post's correction writes into each copy of the post: the time of an edit, or the mark
 *  of a delete, with whether the comment's anchor quote is empty as the delete left it
 *  (`quoteCleared`): the delete empties the quote of an insert suggestion no accept made document
 *  text, pending or rejected, since that quote is the comment's own words, and a quote empty
 *  before the delete stays empty. */
type PostMark =
  | { readonly kind: "edited"; readonly at: string }
  | { readonly kind: "deleted"; readonly deletion: PostDeletion; readonly quoteCleared: boolean };

/** The document version a comment's delete renamed, as its event names it: the version an
 *  accepted suggestion of the comment had named after the comment's body. */
type RenamedVersion = NonNullable<CommentEventPayload["renamed_version"]>;

/** The post a correction event is of, by the kind and the id each copy of it carries. */
interface CorrectedPost {
  readonly kind: "comment" | "message";
  readonly id: string;
  /** What every block of the post's node id starts with: a block's is `<kind>/<id>#<block id>`. */
  readonly blockNodes: string;
  readonly mark: PostMark;
  /** Whether an address a names read holds names the post or a block of it
   *  (`correctedPostAddresses`); undefined for an event whose id is no post's. */
  readonly names: ((address: string) => boolean) | undefined;
  /** The version a comment's delete renamed (`renamed_version`), whose old name was the comment's
   *  words; undefined for every other correction, and for a delete that renamed none. */
  readonly renamedVersion: RenamedVersion | undefined;
}

/** What one family's cached answer becomes once `post` is corrected: `answer`, as the query keyed
 *  `queryKey` holds it, with the correction's facts in each copy of the post it holds; undefined
 *  where it holds none. */
type FamilyWrite = (answer: unknown, post: CorrectedPost, queryKey: QueryKey) => unknown;

/** What a correction makes of a refusal a family's read holds as its error, which lists items to
 *  choose between, a post among them: the refusal with the post's item corrected; undefined where
 *  it lists none, or the post's item already reads as the correction leaves it. */
type RefusalWrite = (refusal: ApiError, post: CorrectedPost) => ApiError | undefined;

/** The writer of an ask's thread, by its own read (`["ask", id]`) or the Inbox's
 *  (`askThreadQuery`): its thread's replies. */
const askThreadWrite = ofKind("comment", (answer, post) =>
  markedAskThread(answer as AskThread, post)
);

/**
 * Every query family whose answer holds a comment or an issue message, or a copy of one's body or
 * marks, with what a correction writes into it (`markCorrectedPost`). A family is named as census
 * query C-Q1 names it (class C of dispatch://INFRA-1523's surface census): the key's first
 * element, and its second where the key writes one as a string, joined by a space
 * (`["references", "to", …]` is `references to`). Every family the web app's source keys a query
 * under is here or in `NO_POST_FAMILIES`, and nothing else is in either
 * (`corrected-post-families.test.ts`).
 */
export const CORRECTED_POST_FAMILIES: Readonly<Record<string, FamilyWrite>> = {
  /** An ask's read (`["ask", id]`): its thread's replies. */
  ask: askThreadWrite,
  /** An Inbox thread's ask (`askThreadQuery`): its thread's replies. */
  "ask-thread": askThreadWrite,
  /** An issue's asks (`["asks", key, …]`), each with its thread where the read asked for it. */
  asks: ofKind("comment", markedAskRows),
  /** A session's conversation (`agentMessagesQuery`, `["agents", session, "messages"]`), which
   *  lists each issue message sent to it; `["agents"]` itself lists the sessions. */
  agents: ofKind("message", (answer, post, queryKey) =>
    queryKey[2] === "messages" ? markedMessageReads(answer, post) : undefined
  ),
  /** A document's own read (`["artifact", id]`), the name of each of its versions among it, a
   *  read of one version (`["artifact", id, "version", n]`, and `["artifact", id, n]` where a
   *  reference pins version n) with that version's name, its comments
   *  (`["artifact", id, "comments"]`) and its asks with their threads (`["artifact", id, "asks"]`);
   *  the document's other reads hold no post. */
  artifact: ofKind("comment", (answer, post, queryKey) => {
    if (queryKey.length === 2) {
      return withRenamedVersion(answer as Artifact, post);
    }
    switch (queryKey[2]) {
      case "comments":
        return markedComments(answer, post);
      case "asks":
        return markedAskRows(answer, post);
      default:
        return renamedVersionRead(answer as ArtifactVersionContent, post, queryKey);
    }
  }),
  /** A project document read by its project and slug (`["artifact-ref", "<project>/<slug>"]`):
   *  the name of each of its versions. */
  "artifact-ref": ofKind("comment", (answer, post) => withRenamedVersion(answer as Artifact, post)),
  /** A comment's read (`["comment", id]`): the comment and its replies. */
  comment: ofKind("comment", (answer, post) => markedCommentRead(answer as CommentRead, post)),
  /** An issue's comments, or a document's on it (`["comments", key, …]`). */
  comments: ofKind("comment", markedComments),
  /** An issue's event log (`["events", key]`, pages), its unread line and head, and the margin's
   *  pinned events: each event of the post's own that carries its body. */
  events: (answer, post) => {
    const marked = (event: DispatchEvent) => markedEvent(event, post);
    return isPages<DispatchEvent[]>(answer)
      ? markedPages(answer, (page) => markEach(page, marked))
      : markEach(rowsOf<DispatchEvent>(answer), marked);
  },
  /** The Inbox's ask rows (`inboxQuery`, `["inbox", project]`), each with its thread. */
  inbox: ofKind("comment", markedAskRows),
  /** An issue's details (`["issue", key]`): the name of each version of each of its documents.
   *  Its references read (`["issue", key, "references"]`, the Artifacts tab's): each row of
   *  `links` and `linked_from` whose node is the post or a block of it, which the row names by the
   *  head of its words. The other reads under it (coverage, results, collaborators and steps)
   *  hold no comment or message. */
  issue: (answer, post, queryKey) => {
    if (queryKey.length === 2) {
      return post.renamedVersion === undefined
        ? undefined
        : renamedVersionInIssue(answer as IssueDetails, post);
    }
    return queryKey[2] === "references"
      ? markedIssueReferences(answer as IssueReferencesAnswer, post)
      : undefined;
  },
  /** A message's read (`["issue-message", key, id]`): the message and its replies. */
  "issue-message": ofKind("message", (answer, post) =>
    markedMessageRead(answer as MessageRead, post)
  ),
  /** An issue's messages (`["issue-messages", key]`) and the margin's pinned ones
   *  (`["issue-messages", key, "margin-pinned", …]`). */
  "issue-messages": ofKind("message", markedMessageReads),
  /** The reader's answers and ask replies (`["me", "answers"]`, pages): each reply's body. */
  "me answers": ofKind("comment", (answer, post) =>
    isPages<MyAnswersResponse>(answer)
      ? markedPages(answer, (page) => markedAnswerPage(page, post))
      : undefined
  ),
  /** The reader's thread rows (`["me", "replies", …]`): the Inbox's, what they dismissed (pages),
   *  each owner's unread set and the count's one-row page. */
  "me replies": (answer, post) =>
    isPages<MyRepliesResponse>(answer)
      ? markedPages(answer, (page) => markedThreadPage(page, post))
      : isThreadPage(answer)
        ? markedThreadPage(answer, post)
        : undefined,
  /** A person's page (`["people", login, "nodes"]`, pages): each row of the post, which the page
   *  names by the first line of its words (`PersonPage`). */
  people: (answer, post) =>
    isPages<PersonNodes>(answer)
      ? markedPages(answer, (page) => markedPersonPage(page, post))
      : undefined,
  /** Each names read (`referenceTargetsQuery`): every member naming the post or a block of it. */
  "reference-targets": (answer, post) =>
    post.names === undefined
      ? undefined
      : markedNamesRead(answer as ReferenceTargetsAnswer | undefined, post.names, post.mark),
  /** A graph read (`referencesQuery`, `["references", "to", …]`): its node where that is the post
   *  or a block of it, and each edge whose other end is, as the Referenced by panel lists them. A
   *  refusal of it that lists a post is written too (`REFUSAL_FAMILIES`). */
  "references to": (answer, post) => markedGraphRead(answer as GraphReferences, post),
  /** The search palette's answers (`["search", …]`): each hit of the post. */
  search: (answer, post) => markedSearch(answer as SearchResponse, post),
};

/**
 * Every query family of `CORRECTED_POST_FAMILIES` whose read Dispatch can refuse with a list of
 * items to choose between, posts among them, with what a correction writes into such a refusal,
 * which the query holds as its error rather than as an answer (`correctRefusal`). Each has a test
 * of the refusal's write (`corrected-post-families.test.ts`).
 */
export const REFUSAL_FAMILIES: Readonly<Record<string, RefusalWrite>> = {
  /** The graph read every surface resolves an id written short through (`useWholeRouteRead`),
   *  which Dispatch refuses where the id names several items (`ID_AMBIGUOUS`), listing each with
   *  its first line for the match list (`ItemMatchList`). An edit adds its time (`edited_at`) to
   *  the post's item and keeps the first line the refusal gave, which a read gives anew; and a
   *  delete takes the item's first line off beside its mark (`markedRefusal`). */
  "references to": markedRefusal,
};

/**
 * Every other query family the web app's source keys a query under, each with why its answer
 * holds no comment or issue message and no copy of one's body or marks: census row C.15's list,
 * and the families C-Q1 has found since. A family added to the source is in neither table until
 * someone reads its answer and puts it in one (`corrected-post-families.test.ts`).
 */
export const NO_POST_FAMILIES: Readonly<Record<string, string>> = {
  "agent-artifacts": "a session's files and their names",
  "agent-tokens": "the reader's agent tokens: names, scopes and times",
  "agents issue-picker": "the issues the Agents page's picker offers: keys and titles",
  architecture: "a project's architecture components",
  "architecture-sources": "the repositories a project's architecture is read from",
  "artifact-reviews": "a document's review steps and who approved them",
  artifacts: "an issue's documents by name, which prints no version name (census C.15); no comment",
  broadcast:
    "a broadcast's copies, messages on no issue, which Dispatch edits and deletes none of (`NOT_AN_ISSUE_MESSAGE`, census C.13)",
  "broadcast list": "the reader's broadcasts, the same copies on no issue (census C.13)",
  components: "a refresh target alone: no query of this build reads it",
  "credential-capabilities": "what the secrets broker can grant",
  "credential-grants": "the credentials the reader granted",
  "credential-pending": "the credential requests waiting on the reader",
  decisions: "the issue's decisions, read from its decisions document (census 1.16)",
  delivery: "the delivery dashboard's prefix: runs, jobs and measures",
  "delivery measures": "the delivery dashboard's measures",
  "delivery pipeline": "the delivery pipeline's runs and jobs",
  "delivery run": "one deploy run with its jobs",
  "delivery timeline": "the delivery timeline's pull requests and runs",
  "delivery-settings": "the delivery timeline's configuration",
  "doc-summary": "a document's name and owner for a link's card (census C.15)",
  "document-read": "the version of a document the reader last read",
  github: "GitHub's own answer for an unfurl",
  "github-link": "a linked pull request as GitHub answers it",
  "github-link-checks": "a linked pull request's checks as GitHub answers them",
  handoff: "one phase handoff with its body: a handoff is no comment or message",
  handoffs: "an issue's phase handoffs, without their bodies",
  "issue-ancestors": "the parents of the issues a listing holds: keys and titles",
  "issue-summary": "an issue's title and status for a link's card (census C.15)",
  issues: "issue lists: keys, titles, statuses and labels",
  "issues advances": "the issues advancing a priority",
  "issues pinned": "the reader's pinned issues",
  "issues project": "a project's issue list",
  "known-session": "whether Dispatch holds any record of a session, and its title: no post",
  "legion-daemon": "a project's Legion daemon setting",
  "machine-logins": "the reader's machine logins",
  "new-issue-settings": "the label and session a new issue from the bar takes",
  priorities: "the priorities and the work under them",
  "priorities full": "the priorities and the work under them",
  "priorities history": "one priority's history of its own fields",
  "priorities summary": "the two counts of work under no priority and parked under one",
  project: "a project's documents by name, as `artifacts` lists an issue's",
  "project-default-assignees": "each project's default assignee",
  projects: "the projects",
  "references questions": "which asks a source cites, by id (census C.15)",
  related: "issues related by their words, printed by owner label alone (census C.15)",
  signins: "the reader's sign-ins",
  subscribers: "who follows an issue, a document or an ask",
  "user-agent-state": "the reader's read mark, pin and Clear per session",
  "user-state": "the reader's read, pin and dismiss marks per issue",
  users: "everyone who has signed in",
  whoami: "the signed-in principal",
  "whoami-scopes": "the scopes the signed-in person's own credential holds (GET /api/v1/whoami)",
};

/**
 * Writes a post's correction (`comment.edited`, `comment.deleted`, `message.edited`,
 * `message.deleted`) into every copy of the post the cache holds, as the server's own read
 * answers the corrected post. Each of these reads is refreshed by the same event
 * (`eventRefreshTargets`), and its answer replaces this; but a refetch that fails keeps the answer
 * it would have replaced, and a surface drawing that answer shows no error, so without this it
 * would go on printing a deleted post's words, or an edited post's without its mark. The event
 * already holds the deciding fact: the delete's mark (`deleted_at`, `deleted_by`) or the edit's
 * time (`edited_at`).
 *
 * Every family whose answer holds a copy of a post is written, by its writer in
 * `CORRECTED_POST_FAMILIES`. A delete takes every copy of the post's words out and writes its
 * mark beside where they were:
 * - a comment's or a message's row (`body` empty and no `body_tree`, a comment's `suggestion`
 *   null and, where the delete emptied it, its anchor's `quote`), wherever a list, a read, an
 *   ask's thread or a session's conversation holds it;
 * - the quote a reply in any of those rows holds of a block of the post (`reply_block`): removed,
 *   with no text, as a read of the reply answers it;
 * - each event of the post's own that carries its body, in an issue's log and the margin's pinned
 *   events: no `body`, `body_truncated`, `suggestion` or body tree, and the anchor quote the
 *   delete emptied, as the server's read of the event log gives it (`attachPostMarks` in
 *   `api/events.go`), which leaves the previews a reply's events froze of the post;
 * - a thread row of the reader's (`["me", "replies", …]`) whose root is the post (`subject`
 *   empty) or whose latest reply is (`reply.first_line` empty), and a reply of the reader's
 *   answers (`["me", "answers"]`, `body` empty);
 * - a row of a person's page (`["people", login, "nodes"]`) that is the post: an empty `title`;
 * - a search hit of the post (`["search", …]`): an empty `snippet` and no `snippet_tree`;
 * - a graph read (`["references", "to", …]`) whose node is the post or a block of it, and each of
 *   its edges whose other end is: the node with no text, no tree and no pinned text, and an edge
 *   with no excerpt. The server answers a read of a deleted post's block 404, which no cached
 *   answer holds, and the node's mark is what a reference to the block reads the deletion from;
 * - each row of an issue's references read (`["issue", key, "references"]`) whose node is the
 *   post or a block of it: the node marked as a graph read's is, and the row's `name`, the head
 *   of the post's words, empty;
 * - each item a refusal of a graph read lists that is the post: Dispatch refuses the read of an id
 *   written short where the id names several items (`ID_AMBIGUOUS`), listing each with its first
 *   line for the reader to choose from, and the query holds that refusal as its error
 *   (`REFUSAL_FAMILIES`). The item loses its `first_line` beside the delete's mark, as the
 *   prefix layer lists a deleted post (`refs.Candidate` in `refs/prefix.go`), and the refusal
 *   stays a refusal, so the list stays drawn (`correctRefusal`);
 * - each member of a names read naming the post or a block of it (`["reference-targets", …]`,
 *   by `correctedPostAddresses`): the post's named `""`, a block's gone, with no tree;
 * - the name of the document version a comment's accepted suggestion had named after the
 *   comment's body, where the delete renamed it (`renamed_version`): that version's `summary`, in
 *   the document's own read (`["artifact", id]`), a project document's read by its slug
 *   (`["artifact-ref", …]`) and the issue's details (`["issue", key]`), each of which lists the
 *   document's versions, and in each read of that version alone (`["artifact", id, "version", n]`,
 *   `["artifact", id, n]`).
 *
 * An edit writes its time (`edited_at`) on each of those copies that carries one, unless the copy
 * carries a later edit's already (`editedAfter`), and nothing else: an edit event's body can be
 * cut short (`body_truncated`), so the words stay those of the answer until a read gives the new
 * ones.
 *
 * A read already out when the event arrives was asked before the correction, so its answer would
 * land the old words over the marks written here, and keep them if the refetch the event owes it
 * then fails. So each answer this writes is put over every read of its query still out
 * (`replaceQueryData`, `query-client.ts`): the read is given up in the same step as the write, and
 * the query is read again once the write is in, by a read asked after the correction. That guard
 * stands on a write of the query, and a read out of a query whose answer this writes nothing -
 * none yet, or one holding no copy of the post - has none, though what it lands can hold the post
 * as it stood before the correction. Giving up every such read would give up reads that cannot
 * hold a post, the issue's steps read (`["issue", key, "steps"]`), whose owner never cancels a
 * read it has out (`ownQuery`), among them. So for those reads alone the correction is kept while
 * any of them is out (`keepCorrection`), and the answer each of them lands is written with the
 * correction's facts.
 *
 * A refusal is no answer, so it is written in place of the one the query holds rather than by
 * `replaceQueryData`. A read out of a query that holds no answer has emptied the query's error
 * (TanStack's `fetchState`), so where such a read follows a failure, which may have been a
 * refusal, the read is given up to put that refusal back, the refusal is written, and the query is
 * read again where a surface draws it, with the written refusal held beside that read
 * (`correctRefusal`). The refusal a read out of any other query of the family lands is written
 * with the correction's facts, as an answer such a read lands is (`keepCorrection`). A query that
 * holds a refusal beside an answer - an id written short that named one item, then several - has
 * its answer written there too, not by `replaceQueryData`, whose `setQueryData` would clear the
 * refusal and draw the prefix as resolved to the one item it named: the refusal stays its error
 * and its status, written, so its list stays drawn, and the answer takes the correction beside
 * it.
 *
 * An optimistic write out when the event arrives read what its rollback puts back before the
 * correction too: a margin action on a comment (`useMarginItems`) puts back the comments it read
 * as it began where Dispatch refuses it, and Dispatch refuses one on a comment deleted meanwhile
 * (`409 POST_DELETED`), so the rollback would put the deleted comment's words back over the
 * delete's mark, and keep them if the read after it fails. The Inbox's snooze, hand-on, answer,
 * dismiss and Mark all read, and an issue's components save, roll back the same way. So the
 * correction is also kept while any such write is pending, and each write by hand
 * (`setQueryData`) into an answer of a written family made meanwhile is written with the
 * correction's facts (`keepCorrection`).
 *
 * A member a names read answered ambiguous or gone names no post, and is left as it is. An
 * answer holding no copy of the post, or no answer yet, is written nothing, and nor is one that
 * already reads as the correction leaves it: an answer written back unchanged would still count as
 * a new one and clear the read's stale mark. A delete's event carries the delete's time, as every
 * one this server writes does; one that carried none would have no mark to write, and writes
 * nothing.
 */
export function markCorrectedPost(queryClient: QueryClient, event: DispatchEvent): void {
  const post = correctedPost(event);
  if (post === undefined) {
    return;
  }
  // The reads of a written family out at the event whose answer this writes nothing. A query it
  // writes is read again by the write, after the correction, and is not one of them.
  const unwritten = new Set<string>();
  for (const query of queryClient.getQueryCache().getAll()) {
    const family = queryFamily(query.queryKey);
    const write = familyWrite(family);
    if (write === undefined) {
      continue;
    }
    const marked = corrected(query, write, [post]);
    const refusal = refusalWrite(family);
    const refused = refusal !== undefined && ambiguousItems(query.state.error) !== undefined;
    if (marked !== undefined && !refused) {
      replaceQueryData(queryClient, query.queryKey, marked);
      continue;
    }
    // A refusal's correction that gave up every read of the query from before the correction, and
    // has it read again where a surface draws it, keeps the correction for none of them.
    const askedAgain =
      refusal !== undefined && correctRefusal(queryClient, query, refusal, post, marked);
    if (!askedAgain && query.state.fetchStatus === "fetching") {
      unwritten.add(query.queryHash);
    }
  }
  // The optimistic writes out at the event: each mutation still pending that holds what its
  // `onMutate` answered (`state.context`), the value TanStack hands its `onError` to roll back
  // to. A mutation that holds none is handed nothing to roll back to.
  const rollbacks = new Set(
    queryClient.getMutationCache().findAll({
      predicate: (mutation) => mutation.state.context !== undefined,
      status: "pending",
    })
  );
  keepCorrection(queryClient, { out: unwritten, post, rollbacks });
}

/** The family of `queryKey`, as `CORRECTED_POST_FAMILIES` names one: its first element, with its
 *  second where the two name a family of their own; undefined for a key no string opens. */
function queryFamily(queryKey: QueryKey): string | undefined {
  const [first, second] = queryKey;
  if (typeof first !== "string") {
    return undefined;
  }
  const pair = typeof second === "string" ? `${first} ${second}` : undefined;
  return pair !== undefined &&
    (Object.hasOwn(CORRECTED_POST_FAMILIES, pair) || Object.hasOwn(NO_POST_FAMILIES, pair))
    ? pair
    : first;
}

/** The writer of the answers of the family `family` (`queryFamily`), or undefined for a family that
 *  holds no post. */
function familyWrite(family: string | undefined): FamilyWrite | undefined {
  return family !== undefined && Object.hasOwn(CORRECTED_POST_FAMILIES, family)
    ? CORRECTED_POST_FAMILIES[family]
    : undefined;
}

/** The writer of a refusal a read of the family `family` (`queryFamily`) can hold, or undefined for
 *  a family whose reads Dispatch refuses with no list of posts (`REFUSAL_FAMILIES`). */
function refusalWrite(family: string | undefined): RefusalWrite | undefined {
  return family !== undefined && Object.hasOwn(REFUSAL_FAMILIES, family)
    ? REFUSAL_FAMILIES[family]
    : undefined;
}

/** The answer `query` holds with each of `posts`' corrections written in, in order, by `write`, the
 *  writer of the family `query` is of; undefined where it holds none, or the corrections leave it
 *  reading as it does. The answer keeps each part the corrections left as it was the cached one's,
 *  so the cache's own structural sharing walks only what they changed. */
function corrected(query: Query, write: FamilyWrite, posts: readonly CorrectedPost[]): unknown {
  const answer = query.state.data;
  if (answer === undefined) {
    return undefined;
  }
  let marked: unknown = answer;
  for (const post of posts) {
    marked = write(marked, post, query.queryKey) ?? marked;
  }
  const shared = replaceEqualDeep(answer, marked);
  return shared === answer ? undefined : shared;
}

/**
 * Writes `post`'s correction, by `write`, into the refusal `query` holds as its error where it
 * lists the post, and `answer`, the answer it holds as the correction leaves it, where the
 * correction changes that (`markCorrectedPost`); answers whether it gave up every read of the
 * query from before the correction and had it read again where a surface draws it: a correction
 * need be kept for no read of it then.
 *
 * The refusal written takes the place of the one the query holds (`setState`), and stays a
 * refusal, so each surface drawing it keeps drawing the list the reader chooses from. TanStack
 * empties a query's error as a read of it that holds no answer starts (`fetchState` in
 * `@tanstack/query-core`'s `query.ts`), and puts back the state from before that read where the
 * read is given up, as `replaceQueryData` relies on. So a read out with no answer, after an earlier
 * read of the query failed, is given up first: the refusal that failure may have been is in the
 * cache again, and is written. An answer the correction writes is put in beside the refusal
 * (`setState`), which stays the query's error and its status, where `setQueryData` would clear
 * the refusal and draw the prefix as resolved; as `replaceQueryData` does, a read of the query out
 * is given up first and the query read again after the write, so the state TanStack puts back
 * where that read is given up is the written one. A read given up is asked again where a surface
 * draws the query (`invalidateQueries`, `refetchType: "active"`), after the correction, and the
 * refusal written is held beside that read, so the cache holds it while the read is out, where a
 * surface that draws a refusal while its read runs again reads it (the issue page's outline,
 * `useAddressFallback`).
 */
function correctRefusal(
  queryClient: QueryClient,
  query: Query,
  write: RefusalWrite,
  post: CorrectedPost,
  answer: unknown
): boolean {
  const { queryKey } = query;
  const { data, errorUpdateCount, fetchStatus } = query.state;
  const emptied = fetchStatus === "fetching" && data === undefined && errorUpdateCount > 0;
  if (emptied) {
    void queryClient.cancelQueries({ exact: true, queryKey });
  }
  const refusal = correctedRefusal(query.state.error, write, [post]);
  const askAgain = emptied || (answer !== undefined && fetchStatus !== "idle");
  // A read given up above leaves none out; this gives up one out beside an answer.
  if (askAgain && query.state.fetchStatus !== "idle") {
    void queryClient.cancelQueries({ exact: true, queryKey });
  }
  if (refusal !== undefined || answer !== undefined) {
    query.setState({
      ...(answer === undefined ? {} : { data: answer, dataUpdatedAt: Date.now() }),
      error: refusal ?? query.state.error,
    });
  }
  if (!askAgain) {
    return false;
  }
  void queryClient.invalidateQueries({ exact: true, queryKey, refetchType: "active" });
  // A read that starts on no answer empties the query's error again (`fetchState`).
  if (refusal !== undefined && query.state.error !== refusal) {
    query.setState({ error: refusal });
  }
  return true;
}

/** `error` with each of `posts`' corrections written in, in order, by `write`, where it is a
 *  refusal the writes change (`REFUSAL_FAMILIES`); undefined for every other error, and where the
 *  corrections leave it reading as it does. */
function correctedRefusal(
  error: unknown,
  write: RefusalWrite,
  posts: readonly CorrectedPost[]
): ApiError | undefined {
  if (!(error instanceof ApiError)) {
    return undefined;
  }
  let refusal = error;
  for (const post of posts) {
    refusal = write(refusal, post) ?? refusal;
  }
  return refusal === error ? undefined : refusal;
}

/**
 * The most corrections a page keeps at once (`keepCorrection`). Each is kept only while a read
 * that was out at its arrival, of a query whose answer it wrote nothing, is still out, or an
 * optimistic write that was out at its arrival is still pending, so a page holds many only under
 * a burst of corrections that arrives while one such read is slow or one such write is out. Past
 * this the oldest goes, and such a read that lands, or such a write's rollback, after a hundred
 * corrections since can bring its post's old words back, until a read of the query after it lands.
 */
const KEPT_CORRECTIONS_MAX = 100;

/** A correction the cache keeps, with what it is kept for: the reads of a written family that were
 *  out when it arrived and whose answer it wrote nothing, by query hash, each until its fetch is no
 *  longer out; and the optimistic writes that were out then, each until it is no longer pending. */
interface KeptCorrection {
  readonly post: CorrectedPost;
  readonly out: Set<string>;
  readonly rollbacks: Set<Mutation>;
}

/** Per QueryClient, the corrections kept, oldest first. */
const keptCorrections = new WeakMap<QueryClient, KeptCorrection[]>();

/**
 * Keeps `correction` while any read of its `out` is still out or any write of its `rollbacks` is
 * still pending, for the cache subscriptions (`keptCorrectionsOf`) to write its facts into what
 * each of them puts in the cache, which was composed before the correction:
 * - `correction.out` holds the reads whose query the correction wrote nothing into
 *   (`markCorrectedPost`). The answer such a read lands is written with the correction's facts,
 *   and so is the refusal it lands where its family's read can be refused with a list of posts
 *   (`REFUSAL_FAMILIES`). No other answer of a read is: a read of a query the correction wrote
 *   is given up by that write (`replaceQueryData`), which leaves no answer of it composed before
 *   the correction to land, and a read that begins after the event is answered as the server
 *   reads the corrected post.
 * - `correction.rollbacks` holds the optimistic writes out at the event: mutations still pending
 *   that hold what their `onMutate` answered, read before the correction, which TanStack hands
 *   their `onError` to put back. `onError` runs before the mutation leaves `pending` (`execute`
 *   in `@tanstack/query-core`'s `mutation.ts`), so while one of them is pending each write by
 *   hand (`setQueryData`) into an answer of a written family is written with the correction's
 *   facts. The cache does not tell which mutation, if any, made a write, so every such write made
 *   then is written. A margin action's rollback (`useMarginItems`) is one such write.
 *
 * Its facts are final - a delete is never taken back, and an edit's time is the time it was made,
 * which never moves a copy's later edit back (`editedAfter`) - so writing them again into an
 * answer composed after the correction, which already reads that way, changes nothing.
 *
 * The bound is the reads and the writes themselves, not a clock: a read that begins after the
 * event is answered as the server reads the corrected post, and a write that begins after it reads
 * the answers the correction wrote, so only one out at the event can put back what predates it. A
 * read is out until its fetch lands, fails, pauses or is cancelled, and the client gives one up
 * once Dispatch says nothing on it for `REQUEST_DEADLINE_MS`, so a read whose Dispatch has gone
 * silent is let go of by that deadline. A clock of that deadline would not be safe on its own: the
 * deadline is on Dispatch's silence, not on the transfer, so a slow answer that keeps arriving
 * outlives it (`client.ts`). A write is pending until its `onSuccess` or `onError`, and its
 * `onSettled`, have run. At most `KEPT_CORRECTIONS_MAX` are kept.
 */
function keepCorrection(queryClient: QueryClient, correction: KeptCorrection): void {
  if (correction.out.size === 0 && correction.rollbacks.size === 0) {
    return;
  }
  const kept = keptCorrectionsOf(queryClient);
  kept.push(correction);
  kept.splice(0, Math.max(0, kept.length - KEPT_CORRECTIONS_MAX));
}

/** The corrections kept for `queryClient`, with the cache subscriptions that write each into the
 *  answer or the refusal a read it was kept for lands and into each write by hand made while a
 *  write it was kept for is pending, and let each go once nothing it was kept for is out. */
function keptCorrectionsOf(queryClient: QueryClient): KeptCorrection[] {
  const existing = keptCorrections.get(queryClient);
  if (existing !== undefined) {
    return existing;
  }
  const kept: KeptCorrection[] = [];
  keptCorrections.set(queryClient, kept);
  const letGo = () => {
    const left = kept.filter(
      (correction) => correction.out.size > 0 || correction.rollbacks.size > 0
    );
    if (left.length !== kept.length) {
      kept.splice(0, kept.length, ...left);
    }
  };
  // The query the subscription is writing the kept corrections into. The cache tells the write's
  // own update before the write returns, and that answer already reads as those corrections leave
  // it, so they are not written into it again.
  let rewriting: Query | undefined;
  queryClient.getQueryCache().subscribe((change) => {
    if (kept.length === 0 || (change.type !== "updated" && change.type !== "removed")) {
      return;
    }
    const { query } = change;
    if (change.type === "updated" && change.action.type === "success" && query !== rewriting) {
      const write = familyWrite(queryFamily(query.queryKey));
      // The corrections kept for this query's own read, and, for a write by hand (`setQueryData`,
      // which the cache tells as a `success` it marks `manual`), those kept for an optimistic write
      // still pending; in the order they arrived.
      const byHand = change.action.manual === true;
      const posts =
        write === undefined
          ? []
          : kept
              .filter(
                (correction) =>
                  correction.out.has(query.queryHash) || (byHand && correction.rollbacks.size > 0)
              )
              .map((correction) => correction.post);
      const marked =
        write === undefined || posts.length === 0 ? undefined : corrected(query, write, posts);
      if (marked !== undefined) {
        const outer = rewriting;
        rewriting = query;
        try {
          queryClient.setQueryData(query.queryKey, marked);
        } finally {
          rewriting = outer;
        }
      }
    }
    if (change.type === "updated" && change.action.type === "error") {
      // A refusal is no answer, so it is put in place of the one landed (`setState`), which the
      // cache tells as no `error` of its own.
      const write = refusalWrite(queryFamily(query.queryKey));
      if (write !== undefined) {
        const posts = kept
          .filter((correction) => correction.out.has(query.queryHash))
          .map((correction) => correction.post);
        const refusal =
          posts.length === 0 ? undefined : correctedRefusal(query.state.error, write, posts);
        if (refusal !== undefined) {
          query.setState({ error: refusal });
        }
      }
    }
    if (change.type === "removed" || query.state.fetchStatus !== "fetching") {
      let shrank = false;
      for (const correction of kept) {
        if (correction.out.delete(query.queryHash)) {
          shrank = true;
        }
      }
      if (shrank) {
        letGo();
      }
    }
  });
  queryClient.getMutationCache().subscribe((change) => {
    const { mutation } = change;
    if (
      kept.length === 0 ||
      mutation === undefined ||
      (change.type !== "removed" && mutation.state.status === "pending")
    ) {
      return;
    }
    let shrank = false;
    for (const correction of kept) {
      if (correction.rollbacks.delete(mutation)) {
        shrank = true;
      }
    }
    if (shrank) {
      letGo();
    }
  });
  return kept;
}

/** The post `event` corrects and what the correction writes; undefined for every other event, and
 *  for an edit or a delete whose event carries no time to write. */
function correctedPost(event: DispatchEvent): CorrectedPost | undefined {
  let kind: CorrectedPost["kind"];
  let mark: PostMark;
  switch (event.type) {
    case "comment.deleted":
    case "message.deleted": {
      const deletion = postDeletion(event.payload);
      if (deletion === null) {
        return undefined;
      }
      kind = event.type === "comment.deleted" ? "comment" : "message";
      // The delete's own payload carries the comment's anchor as the delete left it.
      const anchor = event.type === "comment.deleted" ? event.payload.anchor : undefined;
      mark = { deletion, kind: "deleted", quoteCleared: anchor?.quote === "" };
      break;
    }
    case "comment.edited":
    case "message.edited": {
      const at = event.payload.edited_at;
      if (at === undefined || at === null) {
        return undefined;
      }
      kind = event.type === "comment.edited" ? "comment" : "message";
      mark = { at, kind: "edited" };
      break;
    }
    default:
      return undefined;
  }
  const { id } = event.payload;
  const renamedVersion =
    event.type === "comment.deleted" ? event.payload.renamed_version : undefined;
  return {
    blockNodes: `${kind}/${id}#`,
    id,
    kind,
    mark,
    names: correctedPostAddresses(event),
    renamedVersion,
  };
}

/** `write` for a family whose answers hold posts of `kind` alone: a post of the other kind has no
 *  copy there. */
function ofKind(kind: CorrectedPost["kind"], write: FamilyWrite): FamilyWrite {
  return (answer, post, queryKey) =>
    post.kind === kind ? write(answer, post, queryKey) : undefined;
}

/** `items` with each item `mark` answers for in its place, or undefined where it answers for
 *  none of them (or there are none), so an answer holding no copy of the post is written
 *  nothing. */
function markEach<T>(
  items: readonly T[] | null | undefined,
  mark: (item: T) => T | undefined
): T[] | undefined {
  if (items === null || items === undefined) {
    return undefined;
  }
  const marked = items.map((item) => mark(item));
  return marked.some((item) => item !== undefined)
    ? items.map((item, at) => marked[at] ?? item)
    : undefined;
}

/** A document read - the document's own, or one of an issue's documents - with the version a
 *  comment's delete renamed (`post.renamedVersion`) given the name the delete gave it; undefined
 *  for another document, one that lists no such version or lists it so named already, and every
 *  other correction. */
function withRenamedVersion<D extends Pick<Artifact, "id" | "versions">>(
  document: D,
  post: CorrectedPost
): D | undefined {
  const renamed = post.renamedVersion;
  if (renamed === undefined || document.id !== renamed.artifact_id) {
    return undefined;
  }
  const versions = markEach<Version>(document.versions, (version) =>
    version.number === renamed.number && version.summary !== renamed.summary
      ? { ...version, summary: renamed.summary }
      : undefined
  );
  return versions === undefined ? undefined : { ...document, versions };
}

/** An issue's details with the version a comment's delete renamed given its new name, among the
 *  issue's documents; undefined where the issue lists no such version or it is so named already. */
function renamedVersionInIssue(issue: IssueDetails, post: CorrectedPost): IssueDetails | undefined {
  const artifacts = markEach(issue.artifacts, (artifact) => withRenamedVersion(artifact, post));
  return artifacts === undefined ? undefined : { ...issue, artifacts };
}

/** A read of one version of a document (`["artifact", id, "version", n]`, or `["artifact", id, n]`
 *  where a reference pins version n) with the name a comment's delete gave version n; undefined for
 *  a read of another version or document, a file's version, which carries no name, and a version
 *  so named already. */
function renamedVersionRead(
  read: ArtifactVersionContent,
  post: CorrectedPost,
  queryKey: QueryKey
): ArtifactVersionContent | undefined {
  const renamed = post.renamedVersion;
  const number = queryKey[2] === "version" ? queryKey[3] : queryKey[2];
  if (renamed === undefined || queryKey[1] !== renamed.artifact_id || number !== renamed.number) {
    return undefined;
  }
  return "summary" in read && read.summary !== renamed.summary
    ? { ...read, summary: renamed.summary }
    : undefined;
}

/** Whether `answer` is an infinite read's (`useInfiniteQuery`): its pages, each one request's
 *  answer. */
function isPages<P>(answer: unknown): answer is InfiniteData<P> {
  return (
    typeof answer === "object" &&
    answer !== null &&
    "pages" in answer &&
    Array.isArray(answer.pages)
  );
}

/** `answer` with each page `mark` answers for in its place; undefined where it answers for none. */
function markedPages<P>(
  answer: InfiniteData<P>,
  mark: (page: P) => P | undefined
): InfiniteData<P> | undefined {
  const pages = markEach(answer.pages, mark);
  return pages === undefined ? undefined : { ...answer, pages };
}

/** The delete's mark as a read writes it where the actor is optional: no `deleted_by` for a
 *  delete that names nobody. */
function deletionMarks(deletion: PostDeletion): { deleted_at: string; deleted_by?: Actor } {
  return deletion.by === null
    ? { deleted_at: deletion.at }
    : { deleted_at: deletion.at, deleted_by: deletion.by };
}

/**
 * The time an edit writes over `current`, a copy's own `edited_at`: the edit's, unless the copy
 * already carries a later one. A kept correction is written into the answer a read out at its
 * arrival lands, and into each write by hand made while a write out at its arrival is pending
 * (`keepCorrection`); either can be composed after a later edit, whose event has not arrived yet,
 * and carry that edit's time, which no older edit may move back. The two are ordered as instants
 * (`compareTimestamps`); a time that is no timestamp is not ordered, and the edit's own is
 * written.
 */
function editedAfter(current: string | null | undefined, at: string): string {
  return typeof current === "string" &&
    isTimestamp(current) &&
    isTimestamp(at) &&
    compareTimestamps(current, at) > 0
    ? current
    : at;
}

/** A deleted comment's `anchor` as the delete left it: the quote emptied where the delete left the
 *  comment's empty (`quoteCleared`), since an insert suggestion's quote is the text it put in the
 *  document, the comment's own words; any other quote is the document's own text, and stays. */
function deletedAnchor<A extends { readonly quote?: string }>(anchor: A, quoteCleared: boolean): A {
  return quoteCleared ? { ...anchor, quote: "" } : anchor;
}

/** `block`, the block of its parent a reply names, as a read of the reply answers it once `post`
 *  is corrected; undefined for no block, or one of another post. A block of a deleted post is
 *  removed with no text, beside the delete's mark; one of an edited post carries the edit's time
 *  (`ReplyBlocks` in `refs/reply_blocks.go`). */
function markedReplyBlock(
  block: ReplyBlockWithTrees | null,
  post: CorrectedPost
): ReplyBlockWithTrees | undefined {
  if (block === null || !block.node.startsWith(post.blockNodes)) {
    return undefined;
  }
  if (post.mark.kind === "edited") {
    return { ...block, edited_at: editedAfter(block.edited_at, post.mark.at) };
  }
  const { deleted_by: _by, text_tree: _tree, ...kept } = block;
  return { ...kept, ...deletionMarks(post.mark.deletion), removed: true, text: "" };
}

/** `comment` as a read answers it once `post`, a comment, is corrected: the post itself, or a
 *  reply quoting a block of it; undefined for any other comment. */
function markedComment(
  comment: CommentWithTrees,
  post: CorrectedPost
): CommentWithTrees | undefined {
  const block = markedReplyBlock(comment.reply_block, post);
  if (comment.id !== post.id) {
    return block === undefined ? undefined : { ...comment, reply_block: block };
  }
  if (post.mark.kind === "edited") {
    return { ...comment, edited_at: editedAfter(comment.edited_at, post.mark.at) };
  }
  const { deletion, quoteCleared } = post.mark;
  const { body_tree: _tree, ...kept } = comment;
  return {
    ...kept,
    anchor: kept.anchor === null ? null : deletedAnchor(kept.anchor, quoteCleared),
    body: "",
    deleted_at: deletion.at,
    deleted_by: deletion.by,
    suggestion: null,
  };
}

/** A list of comments - an issue's, or a document's (`["artifact", id, "comments"]`) - once
 *  `post`, a comment, is corrected; undefined where none is the post or quotes it. */
function markedComments(answer: unknown, post: CorrectedPost): CommentWithTrees[] | undefined {
  return markEach(rowsOf<CommentWithTrees>(answer), (comment) => markedComment(comment, post));
}

/** A comment's read, the comment and its replies, once `post` is corrected; undefined where it
 *  holds no copy of the post. */
function markedCommentRead(read: CommentRead, post: CorrectedPost): CommentRead | undefined {
  const root = markedComment(read.comment, post);
  const replies = markEach(read.replies, (reply) => markedComment(reply, post));
  return root === undefined && replies === undefined
    ? undefined
    : { ...read, comment: root ?? read.comment, replies: replies ?? read.replies };
}

/** An ask's thread - an ask's own read, or the thread an ask row carries - once `post`, a
 *  comment, is corrected: its replies; undefined where none is the post or quotes it. */
function markedAskThread<T extends Pick<AskThread, "replies">>(
  thread: T,
  post: CorrectedPost
): T | undefined {
  const replies = markEach(thread.replies, (reply) => markedComment(reply, post));
  return replies === undefined ? undefined : { ...thread, replies };
}

/** A list of ask rows, each with its thread where the read carries one (`InboxRow`,
 *  `IssueAskThreadRow`), once `post` is corrected; undefined where no thread holds a copy. */
function markedAskRows(answer: unknown, post: CorrectedPost): unknown[] | undefined {
  return markEach(rowsOf<{ readonly thread?: AskThread | null }>(answer), (row) => {
    const thread =
      row.thread === undefined || row.thread === null
        ? undefined
        : markedAskThread(row.thread, post);
    return thread === undefined ? undefined : { ...row, thread };
  });
}

/** One page of the reader's answers and ask replies once `post`, a comment, is corrected: each
 *  row whose reply is the post; undefined where none is. */
function markedAnswerPage(
  page: MyAnswersResponse,
  post: CorrectedPost
): MyAnswersResponse | undefined {
  const rows = markEach(page.rows, (row) => markedAnswerRow(row, post));
  return rows === undefined ? undefined : { ...page, rows };
}

/** `row` once `post` is corrected, where its reply is the post: the edit's time, or the delete's
 *  mark with no body; undefined for any other row. */
function markedAnswerRow(row: MyAnswerRow, post: CorrectedPost): MyAnswerRow | undefined {
  const { reply } = row;
  if (reply === undefined || reply.id !== post.id) {
    return undefined;
  }
  if (post.mark.kind === "edited") {
    return { ...row, reply: { ...reply, edited_at: editedAfter(reply.edited_at, post.mark.at) } };
  }
  const { body_tree: _tree, deleted_by: _by, ...kept } = reply;
  return { ...row, reply: { ...kept, ...deletionMarks(post.mark.deletion), body: "" } };
}

/** `message` as a read answers it once `post`, a message, is corrected: the post itself, or a
 *  reply quoting a block of it; undefined for any other message. */
function markedMessage(
  message: MessageWithTrees,
  post: CorrectedPost
): MessageWithTrees | undefined {
  const block = markedReplyBlock(message.reply_block, post);
  if (message.id !== post.id) {
    return block === undefined ? undefined : { ...message, reply_block: block };
  }
  if (post.mark.kind === "edited") {
    return { ...message, edited_at: editedAfter(message.edited_at, post.mark.at) };
  }
  const { body_tree: _tree, ...kept } = message;
  return {
    ...kept,
    body: "",
    deleted_at: post.mark.deletion.at,
    deleted_by: post.mark.deletion.by,
  };
}

/** A message's read, the message and its replies, once `post` is corrected; undefined where it
 *  holds no copy of the post. */
function markedMessageRead(read: MessageRead, post: CorrectedPost): MessageRead | undefined {
  const root = markedMessage(read.message, post);
  const replies = markEach(read.replies, (reply) => markedMessage(reply, post));
  return root === undefined && replies === undefined
    ? undefined
    : { ...read, message: root ?? read.message, replies: replies ?? read.replies };
}

/** A list of message reads - an issue's messages, or a session's conversation - once `post`, a
 *  message, is corrected; undefined where none holds a copy of the post. */
function markedMessageReads(answer: unknown, post: CorrectedPost): MessageRead[] | undefined {
  return markEach(rowsOf<MessageRead>(answer), (read) => markedMessageRead(read, post));
}

/** The events whose payload is a post of each kind with its body, which the server's read of the
 *  event log marks (`postBodyEventKinds` in `api/events.go`). */
const POST_BODY_EVENTS: Record<
  CorrectedPost["kind"],
  Partial<Record<DispatchEvent["type"], true>>
> = {
  comment: {
    "comment.anchor_refreshed": true,
    "comment.answered": true,
    "comment.created": true,
    "comment.edited": true,
    "comment.reopened": true,
    "comment.resolved": true,
    "suggestion.accepted": true,
    "suggestion.rejected": true,
  },
  message: { "message.answered": true, "message.created": true, "message.edited": true },
};

/** An event whose payload is a comment or a message as it stood when the event was written. */
type PostEvent = Extract<
  DispatchEvent,
  { readonly payload: CommentEventPayload | MessageEventPayload }
>;

/** Whether `event` is one whose payload carries the body of a post of `kind`. */
function carriesPostBody(event: DispatchEvent, kind: CorrectedPost["kind"]): event is PostEvent {
  return POST_BODY_EVENTS[kind][event.type] === true;
}

/** The tree an event carries of its payload's body (`api/event_trees.go`). */
const BODY_TREE = "payload.body";

/** `event` as the server's read of the event log answers it once `post` is corrected, where it is
 *  one of the post's own events carrying its body; undefined for every other event. The payload
 *  keeps its type's own shape, which the event's type no longer says once it is rebuilt, hence
 *  the casts. */
function markedEvent(event: DispatchEvent, post: CorrectedPost): DispatchEvent | undefined {
  if (!carriesPostBody(event, post.kind) || event.payload.id !== post.id) {
    return undefined;
  }
  const { mark } = post;
  if (mark.kind === "edited") {
    const editedAt = editedAfter(event.payload.edited_at, mark.at);
    return { ...event, payload: { ...event.payload, edited_at: editedAt } } as DispatchEvent;
  }
  // The body, the mark that it was cut short, and a comment's suggestion, whose replacement text
  // is the comment's text too, as is the anchor quote an insert suggestion's delete emptied.
  const deleted = { deleted_at: mark.deletion.at, deleted_by: mark.deletion.by };
  const trees = withoutBodyTree(event.trees);
  if ("suggestion" in event.payload) {
    const { body: _body, body_truncated: _cut, suggestion: _suggestion, ...kept } = event.payload;
    const anchor =
      kept.anchor === null || kept.anchor === undefined
        ? kept.anchor
        : deletedAnchor(kept.anchor, mark.quoteCleared);
    return { ...event, payload: { ...kept, anchor, ...deleted }, trees } as DispatchEvent;
  }
  const { body: _text, body_truncated: _truncated, ...kept } = event.payload;
  return { ...event, payload: { ...kept, ...deleted }, trees } as DispatchEvent;
}

/** `trees` without the tree of the payload's body. */
function withoutBodyTree(
  trees: Readonly<Record<string, DisplayTree>> | undefined
): Readonly<Record<string, DisplayTree>> | undefined {
  if (trees === undefined || !(BODY_TREE in trees)) {
    return trees;
  }
  const { [BODY_TREE]: _tree, ...kept } = trees;
  return kept;
}

/** Whether `answer` is one page of thread rows (`GET /api/v1/me/replies`). */
function isThreadPage(answer: unknown): answer is MyRepliesResponse {
  return (
    typeof answer === "object" && answer !== null && "rows" in answer && Array.isArray(answer.rows)
  );
}

/** One page of Inbox thread rows once `post` is corrected; undefined where no row of it holds a
 *  copy of the post. */
function markedThreadPage(
  page: MyRepliesResponse,
  post: CorrectedPost
): MyRepliesResponse | undefined {
  const rows = markEach(page.rows, (row) => markedThreadRow(row, post));
  return rows === undefined ? undefined : { ...page, rows };
}

/**
 * An Inbox thread row as `GET /api/v1/me/replies` answers it once `post` is corrected: a row whose
 * root is the post carries the edit's time, or the delete's mark with no subject and the anchor
 * the delete left it (`deletedAnchor`), which the card prints the quote of (`ThreadQuote`), and
 * one whose latest reply is the post carries that reply's delete mark with no first line
 * (`readMyReplyRows` in `api/replies.go`, whose row carries the root comment's anchor). An ask's
 * and a comment's thread take comment replies, a message's message replies. Undefined for any
 * other row, and for a reply's edit, which a row does not mark.
 */
function markedThreadRow(row: MyReplyRow, post: CorrectedPost): MyReplyRow | undefined {
  const replyKind = row.kind === "message" || row.kind === "agent" ? "message" : "comment";
  const root = row.kind === post.kind && row.thread_id === post.id;
  const reply = replyKind === post.kind && row.reply.id === post.id;
  if (post.mark.kind === "edited") {
    return root ? { ...row, edited_at: editedAfter(row.edited_at, post.mark.at) } : undefined;
  }
  if (!root && !reply) {
    return undefined;
  }
  const { deletion, quoteCleared } = post.mark;
  const marks = deletionMarks(deletion);
  let marked = row;
  if (root) {
    const { anchor, deleted_by: _by, ...rootKept } = row;
    marked = {
      ...rootKept,
      ...(anchor === undefined ? {} : { anchor: deletedAnchor(anchor, quoteCleared) }),
      ...marks,
      subject: "",
    };
  }
  if (reply) {
    const { deleted_by: _by, ...replyKept } = row.reply;
    marked = { ...marked, reply: { ...replyKept, ...marks, first_line: "" } };
  }
  return marked;
}

/** One page of a person's nodes once `post` is corrected: each row of the post; undefined where
 *  none is. */
function markedPersonPage(page: PersonNodes, post: CorrectedPost): PersonNodes | undefined {
  const rows = markEach(page.rows, (row) =>
    row.kind === post.kind && row.id === post.id ? markedPersonNode(row, post.mark) : undefined
  );
  return rows === undefined ? undefined : { ...page, rows };
}

/** A row of a person's page that is the post, once `mark` is written: the edit's time, or the
 *  delete's mark with no title, as the server answers the row of a deleted comment, whose cleared
 *  body leaves no first line (`listPersonNodes` in `api/person_nodes.go`). An edit keeps the title,
 *  as every other copy keeps its words, until a read gives the new ones. */
function markedPersonNode(row: PersonNode, mark: PostMark): PersonNode {
  if (mark.kind === "edited") {
    return { ...row, edited_at: editedAfter(row.edited_at, mark.at) };
  }
  const { deleted_by: _by, ...kept } = row;
  return { ...kept, ...deletionMarks(mark.deletion), title: "" };
}

/** A search answer once `post` is corrected: each hit of the post; undefined where none is. */
function markedSearch(answer: SearchResponse, post: CorrectedPost): SearchResponse | undefined {
  const results = markEach(answer.results, (hit) =>
    hit.kind === post.kind && hit.id === post.id ? markedHit(hit, post.mark) : undefined
  );
  return results === undefined ? undefined : { ...answer, results };
}

/** A search hit of the post once it is corrected: the edit's time, or the delete's mark with no
 *  snippet, as the server answers a hit of a deleted post (`stampSearchPostMarks` in
 *  `api/search.go`). */
function markedHit(hit: SearchResult, mark: PostMark): SearchResult {
  if (mark.kind === "edited") {
    return { ...hit, edited_at: editedAfter(hit.edited_at, mark.at) };
  }
  const { deleted_by: _by, snippet_tree: _tree, ...kept } = hit;
  return { ...kept, ...deletionMarks(mark.deletion), snippet: "" };
}

/** A refusal listing the items an id written short names (`ID_AMBIGUOUS`, `ambiguousItems`) once
 *  `post` is corrected (`markedItem`), in a refusal of its own with the refusal's status, code and
 *  words; undefined for any other refusal, and where it lists no item the correction changes. */
function markedRefusal(refusal: ApiError, post: CorrectedPost): ApiError | undefined {
  const candidates = markEach(ambiguousItems(refusal), (item) => {
    const marked = markedItem(item, post);
    return marked === undefined || replaceEqualDeep(item, marked) === item ? undefined : marked;
  });
  return candidates === undefined
    ? undefined
    : new ApiError(
        refusal.status,
        { candidates, code: refusal.code, error: refusal.message, state: refusal.state },
        refusal.retryAfterMs
      );
}

/** An item a refusal lists once `post` is corrected, where it is the post: the post marked
 *  (`markedCandidate`); undefined for every other item. */
function markedItem(item: IDCandidate, post: CorrectedPost): IDCandidate | undefined {
  return item.kind === post.kind && item.id === post.id
    ? markedCandidate(item, post.mark)
    : undefined;
}

/** An item a refusal lists that is the post, once `mark` is written: the edit's time, or the
 *  delete's mark with no first line, as the prefix layer lists a deleted post, whose cleared body
 *  leaves none (`refs.Candidate` in `refs/prefix.go`), keeping the time of an edit before the
 *  delete as it does. */
function markedCandidate(item: IDCandidate, mark: PostMark): IDCandidate {
  if (mark.kind === "edited") {
    return { ...item, edited_at: editedAfter(item.edited_at, mark.at) };
  }
  const { deleted_by: _by, first_line: _words, ...kept } = item;
  return { ...kept, ...deletionMarks(mark.deletion) };
}

/** A graph read (`referencesQuery`) once `post` is corrected: its node, where that is the post or
 *  a block of it, and each edge whose other end is, as the Referenced by panel lists it; undefined
 *  where neither holds a copy. */
function markedGraphRead(read: GraphReferences, post: CorrectedPost): GraphReferences | undefined {
  const node = markedGraphNode(read.node, post);
  const edges = markEach(read.edges, (edge) => markedEdge(edge, post));
  return node === undefined && edges === undefined
    ? undefined
    : { ...read, edges: edges ?? read.edges, node: node ?? read.node };
}

/** `edge` once `post` is corrected, where its other end is the post or a block of it: that end
 *  marked, and, for a delete, no excerpt of its words; undefined for any other edge. */
function markedEdge(edge: GraphEdge, post: CorrectedPost): GraphEdge | undefined {
  // An edge's end is a node as a graph read's own is, less the reading of its text.
  const node = markedGraphNode(edge.node as GraphReferences["node"], post);
  if (node === undefined) {
    return undefined;
  }
  if (post.mark.kind === "edited") {
    return { ...edge, node };
  }
  const { excerpt: _excerpt, ...kept } = edge;
  return { ...kept, node };
}

/** An issue's references read (`["issue", key, "references"]`) once `post` is corrected: each row
 *  of `links` and `linked_from` whose node is the post or a block of it (`markedReferenceLink`);
 *  undefined where none is. */
function markedIssueReferences(
  answer: IssueReferencesAnswer,
  post: CorrectedPost
): IssueReferencesAnswer | undefined {
  const mark = (row: ReferenceLink) => markedReferenceLink(row, post);
  const links = markEach(answer.links, mark);
  const linkedFrom = markEach(answer.linked_from, mark);
  return links === undefined && linkedFrom === undefined
    ? undefined
    : { ...answer, links: links ?? answer.links, linked_from: linkedFrom ?? answer.linked_from };
}

/** `row`, a row of an issue's references read, once `post` is corrected, where its node is the
 *  post or a block of it: that node marked (`markedGraphNode`), and, for a delete, no `name`,
 *  which is the head of the post's words; undefined for any other row. An edit keeps the name, as
 *  a graph read keeps a node's text, until a read gives the new words. */
function markedReferenceLink(row: ReferenceLink, post: CorrectedPost): ReferenceLink | undefined {
  // A row's node is a node as a graph read's own is, less the reading of its text.
  const node = markedGraphNode(row.node as GraphReferences["node"], post);
  if (node === undefined) {
    return undefined;
  }
  return post.mark.kind === "edited" ? { ...row, node } : { ...row, name: "", node };
}

/** A graph node once `post` is corrected, where it is the post or a block of it: the edit's time,
 *  or the delete's mark with no text, no tree and no pinned text. Undefined for any other node. */
function markedGraphNode(
  node: GraphReferences["node"],
  post: CorrectedPost
): GraphReferences["node"] | undefined {
  const ofPost =
    node.kind === "block"
      ? node.id.startsWith(post.blockNodes)
      : node.kind === post.kind && node.id === post.id;
  if (!ofPost) {
    return undefined;
  }
  if (post.mark.kind === "edited") {
    return { ...node, edited_at: editedAfter(node.edited_at, post.mark.at) };
  }
  const { deleted_by: _by, pin, text, text_tree: _tree, ...kept } = node;
  const marked = {
    ...kept,
    ...deletionMarks(post.mark.deletion),
    ...(text === undefined ? {} : { text: "" }),
  };
  if (pin === undefined) {
    return marked;
  }
  const { text_tree: _pinTree, ...pinKept } = pin;
  return { ...marked, pin: { ...pinKept, text: "" } };
}

/** A names read once the post is corrected: each member naming the post or a block of it
 *  (`names`) as GET /api/v1/reference-targets answers it; undefined where no member does. */
function markedNamesRead(
  answer: ReferenceTargetsAnswer | undefined,
  names: (address: string) => boolean,
  mark: PostMark
): ReferenceTargetsAnswer | undefined {
  const targets = markEach(answer?.targets, (target) =>
    target.ambiguous === true || target.gone || !names(target.ref)
      ? undefined
      : mark.kind === "edited"
        ? { ...target, edited_at: editedAfter(target.edited_at, mark.at) }
        : deletedTarget(target, mark.deletion)
  );
  return answer === undefined || targets === undefined ? undefined : { ...answer, targets };
}

/** `target`, a member naming a post or a block of it, as the reference-targets read answers it
 *  once the post is deleted: the post's member named `""`, the body the delete cleared, and a
 *  block's gone, since the delete takes every block out of the post, with no words and no tree;
 *  either carries the delete's mark, and the time of an edit before it as it stood. */
function deletedTarget(target: ReferenceTarget, deleted: PostDeletion): ReferenceTarget {
  const { deleted_by: _by, text_tree: _tree, ...kept } = target;
  return {
    ...kept,
    ...deletionMarks(deleted),
    gone: target.kind === "block",
    name: "",
  };
}
