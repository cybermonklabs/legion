import { snippetSegments } from "@legion/contracts/dispatch-snippet";
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { useQuery } from "@tanstack/react-query";
import { useMemo, useState } from "react";

import { api, isSourceNotFound } from "../../api/client";
import { issueCollaboratorsQuery, usersQuery } from "../../api/queries";
import type { ArtifactWithTrees, GraphReferences, SearchResult } from "../../api/types";
import { useSettled } from "../../hooks/useSettled";
import { useAgents } from "../conversation/useAgents";
import { searchResultKind, stepActive } from "../search/search-model";
import { useAgentText } from "./AgentName";
import { withBlock } from "./block-reference";
import { headRefs } from "./head-refs";
import { type MentionOption, mentionOptions, stoppedRoleOptions } from "./party-options";
import {
  groupRows,
  insertionFor,
  matchesQuery,
  orderedRows,
  REF_PICKER_DEFAULT_WORDS,
  REF_PICKER_QUERY_MIN,
  REF_PICKER_SEARCH_LIMIT,
  REF_PICKER_SEARCHED_KINDS,
  REF_PICKER_TEXT_MAX,
  type RefPickerAgent,
  type RefPickerDoor,
  type RefPickerGroup,
  type RefPickerInsertion,
  type RefPickerKeyResult,
  type RefPickerKind,
  type RefPickerLinks,
  type RefPickerParties,
  type RefPickerParts,
  type RefPickerRow,
  type RefPickerState,
  type RefPickerTrigger,
  type RefPickerWords,
  readTrigger,
} from "./ref-picker";
import { agentArtifactsQuery, architectureQuery } from "./reference-target";
import {
  buildDispatchReference,
  documentRoute,
  parseNodeReference,
  partyOfAddress,
  searchHitRoute,
  shortForm,
} from "./routes";
import { textSourceKind, textSourceOf } from "./text-references";

/** How long after the last keystroke the search goes out, here and in the search palette
 *  (`features/search/SearchPalette.tsx`), so the two surfaces put the same load on the route;
 *  and how long a draft's pills wait before they read it (`draft-pills.tsx`). */
export const SEARCH_DEBOUNCE_MS = 150;

/** What a host tells the picker: the draft it holds, where the caret is in it, and which issue
 *  and project the box belongs to. A ProseMirror or assistant-ui host passes the text of the
 *  block the caret is in and the caret's offset within it, and maps the insertion's offsets
 *  back; a `<textarea>` host passes its value and `selectionStart`. */
export interface RefPickerOptions {
  readonly text: string;
  readonly caret: number;
  /** A new comment that delivers can pick only parties the server can route. */
  readonly delivers?: boolean;
  /** The issue the box writes on, when it has one: its collaborators are the people this box
   *  offers. A box with none offers everyone who has signed in. */
  readonly issueKey?: string;
  /** The project whose components the picker offers; taken from the issue's key when the box
   *  names an issue and no project of its own. */
  readonly project?: string;
  /** False while the host wants no picker - a field that is disabled, or a send in flight. */
  readonly enabled?: boolean;
  /** How the host's text holds a link (`RefPickerLinks`): Markdown, unless the host is a
   *  ProseMirror document, whose links are marks, so its text holds no address a door could
   *  stand in and a party's pick is its mention wherever the caret is. */
  readonly links?: RefPickerLinks;
  /** The words the picker draws (`RefPickerWords`): the hook puts its headings on the groups,
   *  and says its instruction, the line under an opened row with no parts and a role's state
   *  in them. A host with none draws the picker's own (`REF_PICKER_DEFAULT_WORDS`). */
  readonly words?: RefPickerWords;
}

/** What a host needs to draw the picker and to serve its keyboard. */
export interface RefPicker {
  /** Whether a door is open at the caret. */
  readonly open: boolean;
  readonly door: RefPickerDoor | undefined;
  /** The rows to draw, grouped by kind, the groups in this door's order. */
  readonly groups: readonly RefPickerGroup[];
  /** The highlighted row's id, which a host puts in `aria-activedescendant`. */
  readonly activeId: string | undefined;
  /** What the list is while it holds no rows; `RefPickerWords.states` has each one's words. */
  readonly state: RefPickerState;
  /** What the live registry read behind the agent and role rows is, so the reader is never
   *  left reading a missing Agents group as an absence. */
  readonly parties: RefPickerParties;
  /** The one instruction row, while the query is too short for the search route to answer. */
  readonly instruction: string | undefined;
  /** What the list says under the opened row once its parts are read and hold none, in the
   *  words of their kind (`RefPickerWords.parts`); undefined otherwise. */
  readonly partsNote: string | undefined;
  /** What the picker did with a key the host handed it, and the insertion when it took a row. */
  readonly pressKey: (key: string) => RefPickerKeyResult;
  /** Takes the row the reader clicked. */
  readonly take: (id: string) => RefPickerInsertion | undefined;
  /** Highlights the row the pointer is on. */
  readonly highlight: (id: string) => void;
  /** Lists what the row opens, which costs one read: a node's `part_of` backlinks, or an
   *  agent's conversation's artifacts. */
  readonly openParts: (id: string) => void;
}

/** One line of somebody's text, cut to what a row holds where no reference crosses the cut
 *  (`headRefs`), so a row drawn as written never ends in half of one. */
function oneLine(text: string): string {
  return headRefs(text.replace(/\s+/gu, " ").trim(), REF_PICKER_TEXT_MAX, "…");
}

/** What a hit's row is called: the thing's own name where it has one, else the words that
 *  matched, which is all an ask, a comment or a message is named by - and all a hit is named by
 *  when its owner answered no title or name at all. The server marks the matched words in a
 *  snippet; a row draws the text under those marks, as its plain words where the server sent the
 *  snippet's tree (`RefPickerRow.prose`): the snippet is the record's own text, cut, so the row
 *  names the record it is, as the search palette does. */
function hitLabel(hit: SearchResult): Pick<RefPickerRow, "label" | "prose"> {
  const named =
    hit.kind === "issue" && hit.owner.kind === "issue"
      ? hit.owner.title
      : hit.kind === "document"
        ? (hit.artifact?.name ?? (hit.owner.kind === "document" ? hit.owner.name : hit.owner.title))
        : undefined;
  if (named !== undefined) return { label: named };
  const markdown = snippetSegments(hit.snippet)
    .map((segment) => segment.text)
    .join("");
  const label = oneLine(markdown);
  const issue = hit.owner.kind === "issue" ? hit.owner.key : undefined;
  const source = textSourceOf(textSourceKind(hit.kind), hit.id, issue);
  return hit.snippet_tree === undefined
    ? { label }
    : { label, prose: { markdown, source, tree: hit.snippet_tree } };
}

/** The rows the search answered, each the node its hit is (`searchHitRoute`): a document hit is
 *  its document, whose blocks its row opens, whatever block of it the hit matched. A hit whose
 *  `href` names no address is left out, which is how a message with no issue - no address, and
 *  none for its blocks either - is never offered. An issue holds no blocks of its own; its spec
 *  document does. */
function searchRows(hits: readonly SearchResult[]): RefPickerRow[] {
  return hits.flatMap((hit) => {
    const route = searchHitRoute(hit);
    // The route table carries a hit's `kind` as a plain string; the five kinds the route ranks
    // are all the picker asked for (`searchResultKind`, the one narrowing every surface reads
    // a hit's kind through), so a hit of any other kind is no row of its.
    const kind = searchResultKind(hit.kind);
    if (route === undefined || kind === undefined) return [];
    const reference = buildDispatchReference(route);
    const parts: RefPickerParts = { address: reference, kind: "blocks" };
    return [
      {
        detail: shortForm(route),
        id: `ref-picker-${kind}-${hit.id}`,
        kind,
        ...hitLabel(hit),
        reference,
        ...(kind === "issue" ? {} : { parts }),
      },
    ];
  });
}

/** A party's row: a session, or a role a session holds, by the one list the composer's delivery
 *  modes read (`party-options.ts`), so a party offered here and the same party offered as a
 *  mention cannot be two rules about who is running. Each row carries the session it names - the
 *  session itself, or the one holding the role - which the list draws with its running state. A
 *  session's row opens its conversation's documents, as a node's opens its blocks; a role holds
 *  none of its own. */
function partyRow(option: MentionOption, delivers: boolean): RefPickerRow | undefined {
  // A `role:<token>` option names a role; a `session:` one names a session, which is the pair
  // `partyOfAddress` reads an address by (`routes.ts`).
  const party = partyOfAddress(option.target);
  if (delivers && party === undefined) return undefined;
  // A session option's holder is its own name, which is its row's label.
  const agent: RefPickerAgent = {
    sessionId: option.sessionId,
    storedTitle: option.sessionTitle,
    words: option.holder,
  };
  if (party?.kind === "role") {
    return {
      agent,
      detail: `${option.holder} · ${option.detail}`,
      id: `ref-picker-role-${party.role}`,
      kind: "role",
      label: party.role,
      reference: buildDispatchReference({ kind: "role", role: party.role }),
    };
  }
  const reference = buildDispatchReference({ kind: "agent-session", session: option.sessionId });
  return {
    agent,
    detail: option.detail,
    id: `ref-picker-agent-${option.sessionId}`,
    kind: "agent",
    label: option.title,
    parts: { kind: "documents", session: option.sessionId },
    reference,
  };
}

/** The documents of an agent's conversation, newest first as the route lists them: each a
 *  `document` row like an issue's or a project's, named by its file name and written as its own
 *  `dispatch://agent/<session>/artifact/<slug>` address. A file holds no blocks, so its row opens
 *  nothing. */
function documentRows(artifacts: readonly ArtifactWithTrees[]): RefPickerRow[] {
  return artifacts.map((artifact) => {
    const route = documentRoute(artifact);
    return {
      detail: shortForm(route),
      id: `ref-picker-agent-document-${artifact.id}`,
      kind: "document",
      label: artifact.name,
      reference: buildDispatchReference(route),
    };
  });
}

/** The kind a block of a node of `kind` is listed under: an ask's blocks are its parts. */
function blockKindOf(kind: RefPickerKind): RefPickerKind {
  if (kind === "ask") return "ask-part";
  if (kind === "comment") return "comment-block";
  if (kind === "message") return "message-block";
  return "document-block";
}

/** The blocks of one node, from its `part_of` backlinks: an edge of that kind runs from the
 *  block to the node that holds it, so a node's parts are its backlinks of that kind, and each
 *  edge's excerpt carries the block's own first words, with the tree the server read them into
 *  (`RefPickerRow.prose`). A block's record is the one its node names as its parent - its
 *  document, or the ask, comment or message it is a part of - whose document names a `#id` in
 *  it. */
function blockRows(
  holder: string,
  kind: RefPickerKind,
  references: GraphReferences
): RefPickerRow[] {
  const route = parseNodeReference(holder);
  if (route === undefined) return [];
  // A node nothing points at is answered with `edges` null, which is the empty list: no blocks.
  return (references.edges ?? []).flatMap((edge) => {
    const blockId = edge.node.block_id;
    if (edge.kind !== "part_of" || edge.node.kind !== "block" || blockId === undefined) return [];
    const blockRoute = withBlock(route, blockId);
    if (blockRoute === undefined) return [];
    const markdown = edge.excerpt?.text ?? "";
    const tree = edge.excerpt?.text_tree;
    const parent = edge.node.parent;
    const source = textSourceOf(
      textSourceKind(parent?.kind ?? ""),
      parent?.id,
      edge.node.issue_key
    );
    return [
      {
        detail: shortForm(blockRoute),
        id: `ref-picker-block-${blockId}`,
        kind: blockKindOf(kind),
        label: oneLine(markdown),
        reference: buildDispatchReference(blockRoute),
        ...(tree === undefined ? {} : { prose: { markdown, source, tree } }),
      },
    ];
  });
}

/** What the agent and role rows' own source is, which no other read can speak for: a list the
 *  picker is still waiting on, refilling, or unreachable leaves no Agents group, and a missing
 *  group with nothing said about it reads as "no agent". A list that answered is `listed`, and
 *  so is one that neither answered nor is on its way, which no read of it reports. */
function partiesState(live: {
  /** Whether the read answered: the rows are the list it answered with. */
  readonly current: boolean;
  readonly isError: boolean;
  readonly isPending: boolean;
  readonly refilling: boolean;
}): RefPickerParties {
  if (live.current) return "listed";
  if (live.refilling) return "refilling";
  if (live.isPending) return "loading";
  if (live.isError) return "unreachable";
  return "listed";
}

/**
 * The reference picker's rows and its keyboard, over every kind Dispatch holds. Each source is
 * read only while a door is open, and only the search is read per keystroke - debounced, with
 * the door's kinds and a small limit, and never below two characters, which the route refuses.
 * The parties come from the registry the shell already polls, and the people from the issue's
 * collaborators; a project's components are read once and filtered here; a node's blocks are
 * read only when the reader opens that node's row, and an agent's documents only when the reader
 * opens its row.
 */
export function useRefPicker({
  caret,
  delivers = false,
  enabled = true,
  issueKey,
  links = "markdown",
  project,
  text,
  words = REF_PICKER_DEFAULT_WORDS,
}: RefPickerOptions): RefPicker {
  const trigger = enabled ? readTrigger(text, caret, links) : undefined;
  // The trigger the reader dismissed with Escape stays shut while they type on after it: Escape
  // leaves the character they typed, and the picker must not open again on the next letter.
  const [dismissed, setDismissed] = useState<RefPickerTrigger | undefined>(undefined);
  if (dismissed !== undefined && trigger?.start !== dismissed.start) setDismissed(undefined);
  const open = trigger !== undefined && trigger.start !== dismissed?.start;
  const query = open ? trigger.query : "";
  const debounced = useSettled(query, SEARCH_DEBOUNCE_MS);
  const [activeId, setActiveId] = useState<string | undefined>(undefined);
  /** The row whose parts the reader opened - its id, so a row the query lists again is the
   *  same row - while they are open. */
  const [openedId, setOpenedId] = useState<string | undefined>(undefined);
  // A closed picker holds no highlight: the next open starts on its own first row, rather than
  // on a row this open never drew - the same row, listed again, would otherwise be what Enter
  // takes.
  if (!open && activeId !== undefined) setActiveId(undefined);
  if (!open && openedId !== undefined) setOpenedId(undefined);

  const typing = query !== debounced;
  const searching =
    open &&
    !typing &&
    debounced.length >= REF_PICKER_QUERY_MIN &&
    debounced.length <= SEARCH_QUERY_MAX;
  const search = useQuery({
    enabled: searching,
    queryFn: () =>
      api.search(debounced, {
        kind: [...REF_PICKER_SEARCHED_KINDS],
        limit: REF_PICKER_SEARCH_LIMIT,
      }),
    queryKey: ["search", debounced, REF_PICKER_SEARCHED_KINDS, REF_PICKER_SEARCH_LIMIT],
  });
  const live = useAgents(open, true);
  const agentText = useAgentText();
  const readsCollaborators = open && issueKey !== undefined;
  const collaborators = useQuery({
    ...issueCollaboratorsQuery(issueKey),
    enabled: readsCollaborators,
  });
  const readsUsers = open && issueKey === undefined;
  const users = useQuery({ ...usersQuery(), enabled: readsUsers });
  const componentProject =
    project ?? (issueKey === undefined ? undefined : issueKey.slice(0, issueKey.lastIndexOf("-")));
  const readsComponents = open && componentProject !== undefined;
  const architecture = useQuery({
    ...architectureQuery(componentProject),
    enabled: readsComponents,
  });
  // The rows whose source is a list rather than the query: the parties, the people and the
  // components, built once per answer and filtered below. A keystroke changes the query alone,
  // so it re-runs no sort of the live registry and no date of a row it already has.
  const listed = useMemo(() => {
    if (!open) return [];
    // A live list that is not current says nothing about who is absent, so no role is offered as
    // not running under it - the rule the composer's own options follow.
    const stopped = live.current
      ? stoppedRoleOptions(
          live.agents,
          collaborators.data?.agents ?? [],
          agentText,
          words.stoppedRole
        )
      : [];
    const parties: RefPickerRow[] = [];
    for (const option of [...mentionOptions(live.agents, agentText), ...stopped]) {
      const row = partyRow(option, delivers);
      if (row !== undefined) parties.push(row);
    }
    const logins =
      issueKey === undefined
        ? (users.data ?? []).map((user) => user.login)
        : (collaborators.data?.people ?? []).map((person) => person.login);
    // A person's reference costs no read to draw: their login is inside the address.
    const people = logins.map(
      (login): RefPickerRow => ({
        detail: "",
        id: `ref-picker-person-${login}`,
        kind: "person",
        label: login,
        reference: buildDispatchReference({ kind: "person", login }),
      })
    );
    const components = (architecture.data?.components ?? []).map(
      (component): RefPickerRow => ({
        detail: component.id,
        id: `ref-picker-component-${component.id}`,
        kind: "component",
        label: component.title,
        reference: buildDispatchReference({
          id: component.id,
          kind: "component",
          project: componentProject ?? "",
        }),
      })
    );
    return [...parties, ...people, ...components];
  }, [
    agentText,
    architecture.data,
    collaborators.data,
    componentProject,
    delivers,
    issueKey,
    live.agents,
    live.current,
    open,
    users.data,
    words.stoppedRole,
  ]);

  // The parties, the people and the components are listed whole and filtered here; the hits the
  // server ranked for this query are not filtered again (`matchesQuery`).
  const drawn = useMemo(
    () =>
      open
        ? [
            ...listed.filter((row) => matchesQuery(row, query)),
            ...searchRows(search.data?.results ?? []),
          ]
        : [],
    [listed, open, query, search.data]
  );
  // The row whose parts the reader opened, while the list still draws it. What its parts are
  // says what to read - a node's blocks, or an agent's documents - and each read runs only for
  // the row it serves.
  const opened = openedId === undefined ? undefined : drawn.find((row) => row.id === openedId);
  const openedParts = opened?.parts;
  const openedNode = openedParts?.kind === "blocks" ? openedParts.address : undefined;
  const openedSession = openedParts?.kind === "documents" ? openedParts.session : undefined;
  const parts = useQuery({
    enabled: openedNode !== undefined,
    queryFn: () => api.getReferences(openedNode ?? "", { kind: "part_of" }),
    queryKey: ["references", "to", openedNode, "part_of"],
  });
  const documents = useQuery({
    ...agentArtifactsQuery(openedSession),
    enabled: openedSession !== undefined,
  });

  const { partsNote, rows } = useMemo((): {
    readonly partsNote: string | undefined;
    readonly rows: readonly RefPickerRow[];
  } => {
    // An opened row's parts are listed while that row is: they are its, and reached by it. Once
    // their read answers none, the list says so under them.
    if (opened === undefined || openedParts === undefined)
      return { partsNote: undefined, rows: drawn };
    const held =
      openedParts.kind === "documents"
        ? documents.data === undefined
          ? undefined
          : documentRows(documents.data)
        : parts.data === undefined
          ? undefined
          : blockRows(openedParts.address, opened.kind, parts.data);
    return {
      partsNote: held?.length === 0 ? words.parts[openedParts.kind].none : undefined,
      rows: held === undefined ? drawn : [...drawn, ...held],
    };
  }, [documents.data, drawn, opened, openedParts, parts.data, words.parts]);

  const groups = useMemo(
    () => (trigger === undefined || !open ? [] : groupRows(rows, trigger.door, words.headings)),
    [open, rows, trigger, words.headings]
  );
  const ordered = useMemo(() => orderedRows(groups), [groups]);
  const active = ordered.find((row) => row.id === activeId) ?? ordered[0];
  // Each Dispatch read the open depends on, with whether this open asked for it: a read nobody
  // asked for is neither pending nor failed, whatever its own query says while it is disabled.
  // The agent list is not one of them. It is Envoy's list, and its failure - refilling, or the
  // listener unreachable - is said by its own line alone (`parties` below, drawn where its rows
  // would be), never as Dispatch unreachable; a Dispatch that is down fails the reads here as
  // well, since every open asks for its people. While the list is on its way, the picker waits
  // for it as for any read.
  const reads = [
    { asked: searching, failed: search.isError, pending: search.isPending },
    { asked: readsCollaborators, failed: collaborators.isError, pending: collaborators.isPending },
    { asked: readsUsers, failed: users.isError, pending: users.isPending },
    {
      asked: readsComponents,
      // A project with no architecture source answers 404 rather than an empty tree, which is
      // an answer and not a failure (`isSourceNotFound`).
      failed: architecture.isError && !isSourceNotFound(architecture.error),
      pending: architecture.isPending,
    },
    { asked: openedNode !== undefined, failed: parts.isError, pending: parts.isPending },
    {
      asked: openedSession !== undefined,
      failed: documents.isError,
      pending: documents.isPending,
    },
  ];
  const waiting =
    (open && typing && query.length >= REF_PICKER_QUERY_MIN) ||
    (open && live.isPending) ||
    reads.some((read) => read.asked && read.pending);
  const failed = reads.some((read) => read.asked && read.failed);
  const state: RefPickerState =
    ordered.length > 0 ? "rows" : waiting ? "loading" : failed ? "unreachable" : "empty";
  const parties = partiesState(live);

  const insertion = (row: RefPickerRow | undefined): RefPickerInsertion | undefined =>
    row === undefined || trigger === undefined ? undefined : insertionFor(row, trigger, caret);

  const pressKey = (key: string): RefPickerKeyResult => {
    if (!open || trigger === undefined) return { action: "ignored" };
    if (key === "Escape") {
      setDismissed(trigger);
      return { action: "closed" };
    }
    if (key === "ArrowDown" || key === "ArrowUp") {
      if (ordered.length === 0) return { action: "ignored" };
      const index = ordered.findIndex((row) => row.id === active?.id);
      setActiveId(
        ordered[stepActive(index < 0 ? 0 : index, key === "ArrowDown" ? 1 : -1, ordered.length)]?.id
      );
      return { action: "moved" };
    }
    if (key === "ArrowRight" && active?.parts !== undefined) {
      setOpenedId(active.id);
      return { action: "opened-parts" };
    }
    if (key === "ArrowLeft" && openedId !== undefined) {
      setOpenedId(undefined);
      return { action: "opened-parts" };
    }
    if (key === "Enter" || key === "Tab") {
      const taken = insertion(active);
      return taken === undefined ? { action: "ignored" } : { action: "took", insertion: taken };
    }
    return { action: "ignored" };
  };

  return {
    activeId: active?.id,
    door: open && trigger !== undefined ? trigger.door : undefined,
    groups,
    highlight: setActiveId,
    instruction: open && query.length < REF_PICKER_QUERY_MIN ? words.instruction : undefined,
    open,
    openParts: (id: string) => {
      const row = ordered.find((candidate) => candidate.id === id);
      if (row?.parts !== undefined) setOpenedId(row.id);
    },
    parties,
    partsNote,
    pressKey,
    state,
    take: (id: string) => insertion(ordered.find((row) => row.id === id)),
  };
}
