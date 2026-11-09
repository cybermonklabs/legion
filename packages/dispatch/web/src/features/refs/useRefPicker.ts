import { snippetSegments } from "@legion/contracts/dispatch-snippet";
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";

import { api, isSourceNotFound } from "../../api/client";
import { issueCollaboratorsQuery, usersQuery } from "../../api/queries";
import type { GraphReferences, SearchResult } from "../../api/types";
import { useAgents } from "../conversation/useAgents";
import { useAgentText } from "./AgentName";
import { withBlock } from "./block-reference";
import { type MentionOption, mentionOptions, stoppedRoleOptions } from "./party-options";
import {
  groupRows,
  insertionFor,
  matchesQuery,
  orderedRows,
  REF_PICKER_KINDS,
  REF_PICKER_QUERY_MIN,
  REF_PICKER_SEARCH_LIMIT,
  REF_PICKER_SEARCHED_KINDS,
  type RefPickerDoor,
  type RefPickerGroup,
  type RefPickerInsertion,
  type RefPickerKeyResult,
  type RefPickerKind,
  type RefPickerRow,
  type RefPickerState,
  type RefPickerTrigger,
  readTrigger,
  refPickerInstruction,
  stepRow,
} from "./ref-picker";
import { buildDispatchReference, nodeRouteFromHref, parseNodeReference, shortForm } from "./routes";

/** How long after the last keystroke the search goes out. The palette's own wait, so the two
 *  surfaces put the same load on the route. */
const SEARCH_DEBOUNCE_MS = 150;

/** How much of a snippet or of a block's text a row shows: enough to tell two rows apart, short
 *  enough that a row stays one line. */
const ROW_TEXT_MAX = 120;

/** What a host tells the picker: the draft it holds, where the caret is in it, and which issue
 *  and project the box belongs to. A ProseMirror or assistant-ui host passes the text of the
 *  block the caret is in and the caret's offset within it, and maps the insertion's offsets
 *  back; a `<textarea>` host passes its value and `selectionStart`. */
export interface RefPickerOptions {
  readonly text: string;
  readonly caret: number;
  /** The issue the box writes on, when it has one: its collaborators are the people and the
   *  stopped Legion roles this box offers. A box with none offers everyone who has signed in. */
  readonly issueKey?: string;
  /** The project whose components the picker offers; taken from the issue's key when the box
   *  names an issue and no project of its own. */
  readonly project?: string;
  /** The kinds this box offers, where it offers fewer than all of them (`REF_PICKER_KINDS`).
   *  The agent live conversation's box leaves out the message and the message block: a message
   *  of a session's own conversation belongs to no issue, so it has no address to name. */
  readonly kinds?: readonly RefPickerKind[];
  /** False while the host wants no picker - a field that is disabled, or a send in flight. */
  readonly enabled?: boolean;
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
  /** What the list is while it holds no rows; `REF_PICKER_WORDS` has each one's words. */
  readonly state: RefPickerState;
  /** The one instruction row, while the query is too short for the search route to answer. */
  readonly instruction: string | undefined;
  /** The address whose blocks are listed, while the reader has a row open for its parts. */
  readonly openedParts: string | undefined;
  /** What the picker did with a key the host handed it, and the insertion when it took a row. */
  readonly pressKey: (key: string) => RefPickerKeyResult;
  /** Takes the row the reader clicked. */
  readonly take: (id: string) => RefPickerInsertion | undefined;
  /** Highlights the row the pointer is on. */
  readonly highlight: (id: string) => void;
  /** Lists the blocks of the row's node, which costs one read of its `part_of` backlinks. */
  readonly openParts: (id: string) => void;
}

/** One line of somebody's text, cut to what a row holds. */
function oneLine(text: string): string {
  const line = text.replace(/\s+/gu, " ").trim();
  return line.length > ROW_TEXT_MAX ? `${line.slice(0, ROW_TEXT_MAX - 1)}…` : line;
}

/** What a hit's row is called: the thing's own name where it has one, else the words that
 *  matched, which is all an ask, a comment or a message is named by - and all a hit is named by
 *  when its owner answered no title or name at all. The server marks the matched words in a
 *  snippet; a row draws the text under those marks. */
function hitLabel(hit: SearchResult): string {
  const matched = () =>
    oneLine(
      snippetSegments(hit.snippet)
        .map((segment) => segment.text)
        .join("")
    );
  if (hit.kind === "issue" && hit.owner.kind === "issue") return hit.owner.title ?? matched();
  if (hit.kind === "document") {
    const owned = hit.owner.kind === "document" ? hit.owner.name : hit.owner.title;
    return hit.artifact?.name ?? owned ?? matched();
  }
  return matched();
}

/** The rows the search answered. A hit whose `href` names no address is left out, which is how
 *  a message with no issue - no address, and none for its blocks either - is never offered. An
 *  issue holds no blocks of its own; its spec document does. */
function searchRows(hits: readonly SearchResult[]): RefPickerRow[] {
  return hits.flatMap((hit) => {
    const route = nodeRouteFromHref(hit.href);
    // The route table carries a hit's `kind` as a plain string; the five kinds the route ranks
    // are all the picker asked for, so a hit of any other kind is no row of its.
    const kind: RefPickerKind | undefined = REF_PICKER_SEARCHED_KINDS.find(
      (searched) => searched === hit.kind
    );
    if (route === undefined || kind === undefined) return [];
    const reference = buildDispatchReference(route);
    return [
      {
        detail: shortForm(route),
        id: `ref-picker-${kind}-${hit.id}`,
        kind,
        label: hitLabel(hit),
        reference,
        ...(kind === "issue" ? {} : { parts: reference }),
      },
    ];
  });
}

/** A party's row: a session, or a role, by the one list the composer's delivery modes read
 *  (`party-options.ts`), so a party offered here and the same party offered as a mention cannot
 *  be two rules about who is running. */
function partyRow(option: MentionOption): RefPickerRow {
  // A `role:<token>` option names a role; a `session:` one names a session, whose own half of
  // the pair is `sessionOfAddress` (`routes.ts`).
  const role = option.target.startsWith("role:") ? option.target.slice("role:".length) : undefined;
  return role === undefined
    ? {
        detail: option.detail,
        id: `ref-picker-agent-${option.sessionId}`,
        kind: "agent",
        label: option.title,
        reference: buildDispatchReference({ kind: "agent-session", session: option.sessionId }),
      }
    : {
        detail: `${option.holder} · ${option.detail}`,
        id: `ref-picker-role-${role}`,
        kind: "role",
        label: role,
        reference: buildDispatchReference({ kind: "role", role }),
      };
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
 *  edge's excerpt carries the block's own first words. */
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
    return [
      {
        detail: shortForm(blockRoute),
        id: `ref-picker-block-${blockId}`,
        kind: blockKindOf(kind),
        label: oneLine(edge.excerpt?.text ?? ""),
        reference: buildDispatchReference(blockRoute),
      },
    ];
  });
}

/**
 * The reference picker's rows and its keyboard, over every kind Dispatch holds. Each source is
 * read only while a door is open, and only the search is read per keystroke - debounced, with
 * the door's kinds and a small limit, and never below two characters, which the route refuses.
 * The parties come from the registry the shell already polls and from the issue's collaborators;
 * a project's components are read once and filtered here; a node's blocks are read only when the
 * reader opens that node's row.
 */
export function useRefPicker({
  caret,
  enabled = true,
  issueKey,
  kinds = REF_PICKER_KINDS,
  project,
  text,
}: RefPickerOptions): RefPicker {
  const offers = useMemo(
    () => (kind: RefPickerKind) => kinds.some((offered) => offered === kind),
    [kinds]
  );
  // The kinds the route is asked for, which are the searched kinds this box offers: a kind it
  // does not offer is never asked for and never listed.
  const searchedKinds = useMemo(
    () => REF_PICKER_SEARCHED_KINDS.filter((kind) => offers(kind)),
    [offers]
  );
  const trigger = enabled ? readTrigger(text.slice(0, caret)) : undefined;
  // The trigger the reader dismissed with Escape stays shut while they type on after it: Escape
  // leaves the character they typed, and the picker must not open again on the next letter.
  const [dismissed, setDismissed] = useState<RefPickerTrigger | undefined>(undefined);
  if (dismissed !== undefined && trigger?.start !== dismissed.start) setDismissed(undefined);
  const open = trigger !== undefined && trigger.start !== dismissed?.start;
  const query = open ? trigger.query : "";
  const [debounced, setDebounced] = useState("");
  const [activeId, setActiveId] = useState<string | undefined>(undefined);
  const [openedParts, setOpenedParts] = useState<string | undefined>(undefined);
  if (!open && openedParts !== undefined) setOpenedParts(undefined);

  useEffect(() => {
    const timer = window.setTimeout(() => setDebounced(query), SEARCH_DEBOUNCE_MS);
    return () => window.clearTimeout(timer);
  }, [query]);

  const typing = query !== debounced;
  const searching =
    open &&
    !typing &&
    searchedKinds.length > 0 &&
    debounced.length >= REF_PICKER_QUERY_MIN &&
    debounced.length <= SEARCH_QUERY_MAX;
  const search = useQuery({
    enabled: searching,
    queryFn: () => api.search(debounced, { kind: searchedKinds, limit: REF_PICKER_SEARCH_LIMIT }),
    queryKey: ["search", debounced, searchedKinds, REF_PICKER_SEARCH_LIMIT],
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
    enabled: readsComponents,
    queryFn: () => api.getArchitecture(componentProject ?? ""),
    queryKey: ["architecture", componentProject],
  });
  const parts = useQuery({
    enabled: openedParts !== undefined,
    queryFn: () => api.getReferences(openedParts ?? "", { kind: "part_of" }),
    queryKey: ["references", "to", openedParts, "part_of"],
  });

  const rows = useMemo(() => {
    if (!open) return [];
    // A live list that is not current says nothing about who is absent, so no role is offered as
    // not running under it - the rule the composer's own options follow.
    const stopped = live.current
      ? stoppedRoleOptions(live.agents, collaborators.data?.agents ?? [], agentText)
      : [];
    const parties = [...mentionOptions(live.agents, agentText), ...stopped].map(partyRow);
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
    const hits = searchRows(search.data?.results ?? []);
    const opened = hits.find((row) => row.parts === openedParts);
    const blocks =
      openedParts === undefined || parts.data === undefined || opened === undefined
        ? []
        : blockRows(openedParts, opened.kind, parts.data);
    // The parties, the people and the components are listed whole and filtered here; the hits
    // the server ranked for this query are not filtered again (`matchesQuery`). A kind the box
    // does not offer is left out whichever source found it.
    return [
      ...[...parties, ...people, ...components].filter((row) => matchesQuery(row, query)),
      ...hits,
      ...blocks,
    ].filter((row) => offers(row.kind));
  }, [
    agentText,
    architecture.data,
    collaborators.data,
    componentProject,
    issueKey,
    live.agents,
    live.current,
    open,
    offers,
    openedParts,
    parts.data,
    query,
    search.data,
    users.data,
  ]);

  const groups = useMemo(
    () => (trigger === undefined || !open ? [] : groupRows(rows, trigger.door)),
    [open, rows, trigger]
  );
  const ordered = useMemo(() => orderedRows(groups), [groups]);
  const active = ordered.find((row) => row.id === activeId) ?? ordered[0];
  // Each read the open depends on, with whether this open asked for it: a read nobody asked for
  // is neither pending nor failed, whatever its own query says while it is disabled.
  const reads = [
    { asked: searching, failed: search.isError, pending: search.isPending },
    // An agent list that is refilling is a list not ready yet, not a failure to report.
    { asked: open, failed: live.isError && !live.refilling, pending: live.isPending },
    { asked: readsCollaborators, failed: collaborators.isError, pending: collaborators.isPending },
    { asked: readsUsers, failed: users.isError, pending: users.isPending },
    {
      asked: readsComponents,
      // A project with no architecture source answers 404 rather than an empty tree, which is
      // an answer and not a failure (`isSourceNotFound`).
      failed: architecture.isError && !isSourceNotFound(architecture.error),
      pending: architecture.isPending,
    },
    { asked: openedParts !== undefined, failed: parts.isError, pending: parts.isPending },
  ];
  const waiting =
    (open && typing && query.length >= REF_PICKER_QUERY_MIN) ||
    reads.some((read) => read.asked && read.pending);
  const failed = reads.some((read) => read.asked && read.failed);
  const state: RefPickerState =
    ordered.length > 0 ? "rows" : waiting ? "loading" : failed ? "unreachable" : "empty";

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
        ordered[stepRow(index < 0 ? 0 : index, key === "ArrowDown" ? 1 : -1, ordered.length)]?.id
      );
      return { action: "moved" };
    }
    if (key === "ArrowRight" && active?.parts !== undefined) {
      setOpenedParts(active.parts);
      return { action: "opened-parts" };
    }
    if (key === "ArrowLeft" && openedParts !== undefined) {
      setOpenedParts(undefined);
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
    instruction:
      open && query.length < REF_PICKER_QUERY_MIN && searchedKinds.length > 0
        ? refPickerInstruction(kinds)
        : undefined,
    open,
    openedParts,
    openParts: (id: string) => {
      const row = ordered.find((candidate) => candidate.id === id);
      if (row?.parts !== undefined) setOpenedParts(row.parts);
    },
    pressKey,
    state,
    take: (id: string) => insertion(ordered.find((row) => row.id === id)),
  };
}
