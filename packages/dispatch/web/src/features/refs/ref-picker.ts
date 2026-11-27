import { mentionName } from "@legion/contracts/dispatch-mention-sign";

import { AGENT_LIST_REFILLING } from "../../api/client";
import type { DisplayTree, SearchResultKind } from "../../api/types";
import { partyLink } from "./party-mention";
import type { AgentStateWords } from "./SessionName";
import type { TextSource } from "./text-references";

/**
 * The reference picker's rows, their order and the text a pick inserts: one module, so the three
 * hosts that draw it - the shared text field, the document body and the agent live conversation -
 * cannot drift into three pickers. It owns no textarea and no ProseMirror: a host reads the text
 * before its caret, draws the rows this module groups, passes each key to it, and applies the
 * insertion it hands back.
 *
 * `useRefPicker.ts` reads the server for the rows; `RefPickerList.tsx` draws them.
 */

/** Every kind the picker reaches, which is every kind a reference can name, in every box. A block
 *  is its holder's kind in its own right: its rows come from another read (the holder's `part_of`
 *  backlinks) and sit under a heading of their own. A document of an agent's conversation is a
 *  `document` like an issue's or a project's, reached by opening the agent's row
 *  (`GET /api/v1/agents/{session_id}/artifacts`) as a block is reached by opening its holder's:
 *  the search index holds no such file. */
export const REF_PICKER_KINDS = [
  "issue",
  "document",
  "document-block",
  "ask",
  "ask-part",
  "comment",
  "comment-block",
  "message",
  "message-block",
  "component",
  "agent",
  "role",
  "person",
] as const;

export type RefPickerKind = (typeof REF_PICKER_KINDS)[number];

/** The parties: the kinds whose pick inserts a worded link rather than a bare address (R4),
 *  anywhere but a link's address (`insertionText`). */
const REF_PICKER_PARTY_KINDS = ["person", "agent", "role"] as const;

export type RefPickerPartyKind = (typeof REF_PICKER_PARTY_KINDS)[number];

function isPartyKind(kind: RefPickerKind): kind is RefPickerPartyKind {
  return REF_PICKER_PARTY_KINDS.some((party) => party === kind);
}

/** The five kinds `GET /api/v1/search` ranks, which are the kinds the picker cannot list until
 *  the reader has typed: the route refuses a query under two characters. */
export const REF_PICKER_SEARCHED_KINDS: readonly SearchResultKind[] = [
  "issue",
  "document",
  "ask",
  "comment",
  "message",
];

/** The shortest query the search route takes; below it the picker lists only the kinds whose
 *  sources need no query at all (the parties and the components). */
export const REF_PICKER_QUERY_MIN = 2;

/** How many hits one keystroke's search asks for. The picker is a list to pick from, not a
 *  search results page: a reader who cannot see their target in eight rows types another letter.
 */
export const REF_PICKER_SEARCH_LIMIT = 8;

/** How much of a snippet or of a block's text a row shows: enough to tell two rows apart, short
 *  enough that a row stays one line. */
export const REF_PICKER_TEXT_MAX = 120;

/** The heading each kind's rows sit under. */
export const REF_PICKER_HEADINGS: Record<RefPickerKind, string> = {
  agent: "Agents",
  ask: "Asks",
  "ask-part": "Ask parts",
  comment: "Comments",
  "comment-block": "Comment blocks",
  component: "Components",
  document: "Documents",
  "document-block": "Document blocks",
  issue: "Issues",
  message: "Messages",
  "message-block": "Message blocks",
  person: "People",
  role: "Roles",
};

/** What one row of each kind is, which is the label its icon carries for a reader who cannot
 *  see it (`RefKindIcon`). */
export const REF_PICKER_KIND_NAMES: Record<RefPickerKind, string> = {
  agent: "Agent",
  ask: "Ask",
  "ask-part": "Ask part",
  comment: "Comment",
  "comment-block": "Comment block",
  component: "Component",
  document: "Document",
  "document-block": "Document block",
  issue: "Issue",
  message: "Message",
  "message-block": "Message block",
  person: "Person",
  role: "Role",
};

/** Which door the reader opened the picker by. The three differ only in the order their kinds
 *  are ranked in (`kindOrder`): every door reaches every kind. */
export type RefPickerDoor = "all" | "parties" | "issues";

/** The characters that open each door, longest first so `[[` is read before the `[` of nothing.
 *  A door opens only at a word boundary (`readTrigger`). */
export const REF_PICKER_TRIGGERS: readonly {
  readonly chars: string;
  readonly door: RefPickerDoor;
}[] = [
  { chars: "[[", door: "all" },
  { chars: "@", door: "parties" },
  { chars: "#", door: "issues" },
];

/**
 * The order the `[[` door ranks the kinds in, and the tail of the other two doors' order. It
 * leads with the documents because `[[` is the wiki link a writer reaches for to name a page,
 * and because the other two doors are defined by what they lift above this order: `@` the
 * parties, `#` the issues. A block kind follows the kind that holds it, so an opened node's
 * parts are read next to it.
 */
const BASE_KIND_ORDER: readonly RefPickerKind[] = [
  "document",
  "document-block",
  "issue",
  "ask",
  "ask-part",
  "comment",
  "comment-block",
  "message",
  "message-block",
  "component",
  "agent",
  "role",
  "person",
];

/** The kinds in the order this door ranks them: the kinds it lifts, then the rest in the base
 *  order. */
export function kindOrder(door: RefPickerDoor): readonly RefPickerKind[] {
  const lifted: readonly RefPickerKind[] =
    door === "parties" ? REF_PICKER_PARTY_KINDS : door === "issues" ? ["issue"] : [];
  return [...lifted, ...BASE_KIND_ORDER.filter((kind) => !lifted.some((one) => one === kind))];
}

/** What a row opens, for a row that holds anything a reference can name on its own, by what it
 *  holds: a node's blocks, read as the `part_of` backlinks of the node's address, or an agent's
 *  documents, read as its conversation's artifacts by its session. A host opens them with
 *  `openParts`. */
export type RefPickerParts =
  | { readonly kind: "blocks"; readonly address: string }
  | { readonly kind: "documents"; readonly session: string };

/** The session a party's row names: a session's row its own, and a role's row the session that
 *  holds the role. The list draws it through `AgentName`, with its running state, in place of
 *  `words` (`RefRowLabel`, `RefRowDetail`). */
export interface RefPickerAgent {
  readonly sessionId: string;
  /** The session's title as the agent list gave it: its name when the registry has none. */
  readonly storedTitle: string;
  /** The row's own words for the session, as `useAgentText` names it: the whole label of a
   *  session's row, and the head of a role's detail. */
  readonly words: string;
}

/** One row of the picker. `id` is unique within one open: it is the row's React key and the
 *  value a host puts in `aria-activedescendant`. */
export interface RefPickerRow {
  readonly id: string;
  readonly kind: RefPickerKind;
  /** The row's own words - an issue's title, a party's name, a block's first words - which the
   *  browser-side filter matches and the list draws. A row worded with a record's own text is
   *  that text as written, cut where no reference crosses the cut (`headRefs`). */
  readonly label: string;
  /** The line under the label: where the row is, or what it is. Empty for a row with nothing to
   *  add. */
  readonly detail: string;
  /** The `dispatch://` address the row names. */
  readonly reference: string;
  readonly parts?: RefPickerParts;
  /** For a row worded with a record's own text - a hit's matched words, the head of a block - that
   *  text whole, the tree the server read it into, which the list draws as its plain words
   *  (`RefRowLabel`), and the record it is, whose document names a `#id` in it. Absent where the
   *  server sent no tree, and the list draws `label` as written. */
  readonly prose?: {
    readonly markdown: string;
    readonly tree: DisplayTree;
    readonly source: TextSource | undefined;
  };
  /** For a party's row that names an agent, that agent (`RefPickerAgent`). */
  readonly agent?: RefPickerAgent;
}

/** One kind's rows, under that kind's heading. */
export interface RefPickerGroup {
  readonly kind: RefPickerKind;
  readonly heading: string;
  readonly rows: readonly RefPickerRow[];
}

/** The rows grouped by kind, the groups in this door's order, each under its kind's heading of
 *  `headings`; a kind with no row has no group. */
export function groupRows(
  rows: readonly RefPickerRow[],
  door: RefPickerDoor,
  headings: RefPickerWords["headings"] = REF_PICKER_HEADINGS
): RefPickerGroup[] {
  return kindOrder(door).flatMap((kind) => {
    const ofKind = rows.filter((row) => row.kind === kind);
    return ofKind.length === 0 ? [] : [{ heading: headings[kind], kind, rows: ofKind }];
  });
}

/** Every row in the order the reader moves through them, which is the order they are drawn in. */
export function orderedRows(groups: readonly RefPickerGroup[]): RefPickerRow[] {
  return groups.flatMap((group) => [...group.rows]);
}

/** Whether a row the browser filters - a party, a component, a block, all of them listed whole -
 *  matches what the reader has typed. The search hits are not filtered again here: the server
 *  ranked them for this query by meaning, and a second match on the letters alone would drop the
 *  hits meaning search found. */
export function matchesQuery(row: RefPickerRow, query: string): boolean {
  if (query === "") return true;
  const needle = query.toLocaleLowerCase();
  return (
    row.label.toLocaleLowerCase().includes(needle) ||
    row.detail.toLocaleLowerCase().includes(needle) ||
    row.reference.toLocaleLowerCase().includes(needle)
  );
}

/** The door a trigger opened, the characters that opened it, where its first character sits in
 *  the text the host read, and what the reader has typed after it. */
export interface RefPickerTrigger {
  readonly door: RefPickerDoor;
  /** The door's own characters, as the reader typed them: `@`, `#` or `[[`. A host whose
   *  document can move under an open list refuses a pick whose stretch no longer starts with
   *  them (`@legion/proof-editor`'s `insertReference`). */
  readonly chars: string;
  readonly start: number;
  /** Never holds a space or a `]`: a space typed before a row is taken closes the picker, and a
   *  `]` ends the link's words the trigger stood in. */
  readonly query: string;
  /** Whether the trigger stands in a written link's address (`[the plan](#pl`): a pick there
   *  writes the address alone, since the link around it already has its words and a mention
   *  there would be a link inside a link. */
  readonly inAddress: boolean;
}

/** A trigger opens only at a word boundary - the start of the text, or after a space, a line
 *  break or an opening bracket or quote - so `issue#5` opens nothing, and a `#` that starts a
 *  line is still the Markdown heading it becomes as soon as the reader types the space. */
function atWordBoundary(before: string, start: number): boolean {
  if (start === 0) return true;
  return /[\s([{"']/u.test(before.charAt(start - 1));
}

/** The text after a caret that stands in a link's words: the rest of those words, then the `](`
 *  that closes them. The words hold no line break and no bracket but an escaped one
 *  (`partyLink`), so text after a caret that only comes before some later link does not match. */
const LINK_WORDS_AHEAD = /^(?:[^[\]\\\n]|\\.)*\]\(/u;

/** The text before a caret that stands in a link's address: the `](` that opens it, then no
 *  space and no `)` since. Read of the text before a trigger, it says the trigger was typed in a
 *  link's address. */
const LINK_ADDRESS_BEHIND = /\]\([^\s)]*$/u;

/** The text after a caret that ends the address the caret stands in: the space, the line break
 *  or the `)` that ends it, or the end of the text. */
const ADDRESS_END_AHEAD = /^(?:[\s)]|$)/u;

/** How a host's text holds a link. `markdown`: a textarea or an assistant-ui composer,
 *  whose text is the Markdown the reader types, so a link's words and its address are
 *  characters of it. `marks`: a ProseMirror document, where a link is a mark over its words and
 *  the address is the mark's, so the text holds neither, and a `[` or a `](` in it is only the
 *  characters the reader typed. */
export type RefPickerLinks = "markdown" | "marks";

/**
 * The door open at `caret` in `text`, read from the text before it: the last trigger the reader
 * typed that still stands - at a word boundary, with no space typed since. Undefined when none
 * does, which is how the picker closes on a space and how it never opens mid-word. Nor does it
 * stand past a `]`, nor, in a host whose links are Markdown, anywhere in a written link's words:
 * the `@` a mention's words start with (`[@Planner](dispatch://agent/…)`) follows a `[`, and the
 * picker must open neither over the mention it has just written nor where the reader moves the
 * caret into one, since a pick there would write a link inside a link. In such a link's address
 * a door opens on a trigger typed in that address - `[the plan](#pl`, `[the plan](@`,
 * `[the plan]([[go` - and says so (`inAddress`), so its pick writes the address alone; a trigger
 * before the address's first character is not this door, which is why the caret in a mention's
 * own address opens nothing. A pick writes over the trigger through the caret and no further, so
 * such a door stands only where the caret ends the address - at the end of the text, or before
 * the space or the `)` that ends it - where the pick writes the address whole: before the rest
 * of an address it would leave that rest behind its own (`[the plan](dispatch://CORE-1an)`),
 * and once the caret is past the `)` that closes an address, a trigger in that address is no
 * door at all (`[the plan](#plan)` with the caret at its end). A host whose links are marks has
 * no link in its text to stand in, so none of this reads it and no trigger stands in an
 * address. The caret defaults to the end of the text.
 */
export function readTrigger(
  text: string,
  caret = text.length,
  links: RefPickerLinks = "markdown"
): RefPickerTrigger | undefined {
  const markdown = links === "markdown";
  if (markdown && LINK_WORDS_AHEAD.test(text.slice(caret))) return undefined;
  const before = text.slice(0, caret);
  // The address the caret stands in begins after the `](` that opens it.
  const opened = markdown ? LINK_ADDRESS_BEHIND.exec(before) : null;
  if (opened !== null && !ADDRESS_END_AHEAD.test(text.slice(caret))) return undefined;
  const address = opened === null ? undefined : opened.index + "](".length;
  let found: RefPickerTrigger | undefined;
  for (const { chars, door } of REF_PICKER_TRIGGERS) {
    const start = before.lastIndexOf(chars);
    if (start < 0 || (found !== undefined && start < found.start)) continue;
    if (address !== undefined && start < address) continue;
    // A trigger typed in an address the caret has since left, past its `)`.
    if (markdown && address === undefined && LINK_ADDRESS_BEHIND.test(before.slice(0, start))) {
      continue;
    }
    if (!atWordBoundary(before, start)) continue;
    const query = before.slice(start + chars.length);
    if (!/[\s\]]/u.test(query)) {
      found = { chars, door, inAddress: address !== undefined, query, start };
    }
  }
  return found;
}

/** What a pick writes into the draft, and the stretch of it the write replaces: the trigger
 *  character the reader typed through the caret. The text after the caret is untouched, which is
 *  what keeps a pick in the middle of a sentence from eating the rest of it. */
export interface RefPickerInsertion {
  readonly text: string;
  readonly start: number;
  readonly end: number;
  /** The characters the picker opened on, which `[start, end)` begins with: a host whose
   *  document other writers share hands them on, so a stretch that has moved under the open
   *  list is refused rather than written over (`RefPickerTrigger.chars`). */
  readonly trigger: string;
  /** A mention's pick, for a host that writes marks rather than Markdown: the words the mention
   *  shows and the address it names (R4). `text` is that same mention written as Markdown,
   *  which is what a textarea host writes; a ProseMirror host writes these two instead, a
   *  link mark over the words, since plain text holding that Markdown is stored escaped. A party
   *  picked in a link's address is its bare address, as any other pick is, and carries none. */
  readonly link?: { readonly href: string; readonly words: string };
}

/** The text a pick of `row` at `trigger` inserts: a party as its mention (`partyLink`); every
 *  other kind, and any kind in a link's address, as the bare address, which the renderer draws by
 *  its target's own title (R8) and which there completes the link the reader is writing. */
export function insertionText(
  row: RefPickerRow,
  trigger: Pick<RefPickerTrigger, "inAddress">
): string {
  return isPartyKind(row.kind) && !trigger.inAddress
    ? partyLink(row.label, row.reference)
    : row.reference;
}

/** The insertion a pick of `row` makes in a draft whose caret is at `caret`. */
export function insertionFor(
  row: RefPickerRow,
  trigger: RefPickerTrigger,
  caret: number
): RefPickerInsertion {
  const text = insertionText(row, trigger);
  const insertion = { end: caret, start: trigger.start, text, trigger: trigger.chars };
  // A pick that writes more than its bare address writes a mention, which a host that writes
  // marks takes apart. Its words are the characters of the mark's text, which the document's own
  // Markdown writer escapes as it stores them, so they are the name as it is.
  return text === row.reference
    ? insertion
    : { ...insertion, link: { href: row.reference, words: mentionName(row.label) } };
}

/** `insertion` applied to a plain string, with the caret that follows it: what a textarea
 *  host writes back. A ProseMirror or assistant-ui host maps the same offsets into its own
 *  document instead. */
export function applyInsertion(
  text: string,
  insertion: RefPickerInsertion
): { readonly text: string; readonly caret: number } {
  return {
    caret: insertion.start + insertion.text.length,
    text: text.slice(0, insertion.start) + insertion.text + text.slice(insertion.end),
  };
}

/** What the picker did with a key the host handed it. Anything but `ignored` is the picker's:
 *  the host stops the key, and on `took` applies the insertion. */
export type RefPickerKeyResult =
  | { readonly action: "ignored" }
  | { readonly action: "moved" }
  | { readonly action: "closed" }
  | { readonly action: "opened-parts" }
  | { readonly action: "took"; readonly insertion: RefPickerInsertion };

/** Whether a key belongs to an input method's composition - the Enter that confirms a Japanese,
 *  Chinese or Korean word, the Escape that cancels it, the arrows that move between its
 *  segments - which a host leaves to the input method and never hands the picker.
 *  `isComposing` marks a key of a composition where the browser keeps the UI Events order.
 *  Safari on macOS ends the composition before the keydown that confirms it, so that keydown
 *  carries `isComposing` false and the IME key code 229 (WebKit bug 165004), which is why the
 *  code is read too. `keyCode` is deprecated in the DOM types, and that is accepted here: it is
 *  the only signal Safari gives. */
export function isCompositionKey(event: globalThis.KeyboardEvent): boolean {
  return event.isComposing || event.keyCode === 229;
}

/** What the list is, while it is not a list of rows. A host draws the words below. */
export type RefPickerState = "rows" | "loading" | "empty" | "unreachable";

/** The words of each state the list has no rows for, in the composer's own shapes
 *  (`MentionComposer.tsx`: "Loading live agents…", "No live agents match.", "Could not load live
 *  agents — Envoy listener unreachable."). `unreachable` is a Dispatch read that failed: the
 *  agent list is Envoy's, and where it stands is said by its own line (`RefPickerParties`). */
export const REF_PICKER_WORDS: Record<Exclude<RefPickerState, "rows">, string> = {
  empty: "No reference matches.",
  loading: "Loading references…",
  unreachable: "Could not load references — Dispatch unreachable.",
};

/** What the live registry read behind the agent and role rows is, while it is not a list the
 *  picker can draw from. Every view that reads that list says which of these holds where it
 *  would otherwise look like an absence - the Agents page, the live view, an ask's recipients -
 *  and the picker, where the agents and the roles would be, is one of them. `listed` is a read
 *  that answered: the rows are the list. */
export type RefPickerParties = "listed" | "loading" | "refilling" | "unreachable";

/** The words of the two party states the shared refilling notice does not own, in the shapes
 *  the composer's own suggestion list used before the picker replaced it. */
export const REF_PICKER_PARTY_WORDS: Record<"loading" | "unreachable", string> = {
  loading: "Loading live agents…",
  unreachable: "Could not load live agents — Envoy listener unreachable.",
};

/** The words of each kind of parts: the control on a row that holds them (`RefPartsControl`),
 *  and the line under a row whose parts the reader opened and whose read answered none - an
 *  agent's conversation that holds no file, or a node that holds no block. Without that line an
 *  opened row that lists nothing reads as a control that did nothing. */
export const REF_PICKER_PARTS_WORDS: Record<
  RefPickerParts["kind"],
  { readonly control: string; readonly none: string }
> = {
  blocks: { control: "Blocks", none: "No blocks." },
  documents: { control: "Documents", none: "No documents." },
};

/** What the instruction below calls one searched kind's rows. */
const SEARCHED_KIND_WORDS: Record<SearchResultKind, string> = {
  ask: "asks",
  comment: "comments",
  document: "documents",
  issue: "issues",
  message: "messages",
};

const SEARCHED_WORDS = REF_PICKER_SEARCHED_KINDS.map((kind) => SEARCHED_KIND_WORDS[kind]);

/**
 * The one row the picker shows for the kinds it cannot list yet: the search route refuses a
 * query under two characters, so until the reader has typed two the list holds the parties and
 * the components, and this says what the next two letters will add. Every box offers every
 * kind, so every box says the same.
 */
export const REF_PICKER_INSTRUCTION = `Type two characters to search ${SEARCHED_WORDS.slice(0, -1).join(", ")} and ${SEARCHED_WORDS.at(-1)}.`;

/** What a role's row says after its role where no running session holds it: a mention of it is
 *  refused rather than kept for its next session, so it is not told. */
const REF_PICKER_STOPPED_ROLE = "not running, not told";

/**
 * Every word the picker draws, by what draws it: the list's accessible name (`label`), the
 * heading over each kind's rows, the instruction row, each kind of parts' control and the line
 * under an opened row that holds none, the line of each listing state with no rows, the line of
 * each state of the agent and role rows' own source, and what a role's row says of a role no
 * running session holds (`stoppedRole`). A host that draws the picker in words of its own - the
 * issue page's box, through its look (`OutlineLook.picker`) - hands the field a table of its own;
 * every other host draws `REF_PICKER_DEFAULT_WORDS`.
 */
export interface RefPickerWords {
  readonly label: string;
  readonly headings: Readonly<Record<RefPickerKind, string>>;
  readonly instruction: string;
  readonly parts: Readonly<
    Record<RefPickerParts["kind"], { readonly control: string; readonly none: string }>
  >;
  readonly states: Readonly<Record<Exclude<RefPickerState, "rows">, string>>;
  readonly parties: Readonly<Record<Exclude<RefPickerParties, "listed">, string>>;
  readonly stoppedRole: string;
  /** What the state mark before the agent a party's row names says (`AgentName`'s
   *  `stateWords`); with none - `REF_PICKER_DEFAULT_WORDS` gives none - `AgentName`'s own. */
  readonly marks?: AgentStateWords;
  /** What a party's row names its agent by, as the image the row draws that agent as
   *  (`RowAgent`) and at the head of a role's detail: `"shown"`, the name the row draws
   *  (`AgentText.sessionShown`), which inside `NoTooltips` is the page's word for a session nobody
   *  titled; with none - `REF_PICKER_DEFAULT_WORDS` gives none - the session's text
   *  (`AgentText.sessionText`), its short form `session:<8>…` for a session nobody titled. */
  readonly sessionNames?: "shown";
}

/** The picker's own words, which every host but the issue page's box draws: the constants above,
 *  and the refilling notice every view of the agent list says (`AGENT_LIST_REFILLING`). */
export const REF_PICKER_DEFAULT_WORDS: RefPickerWords = {
  headings: REF_PICKER_HEADINGS,
  instruction: REF_PICKER_INSTRUCTION,
  label: "References",
  parties: { ...REF_PICKER_PARTY_WORDS, refilling: AGENT_LIST_REFILLING },
  parts: REF_PICKER_PARTS_WORDS,
  states: REF_PICKER_WORDS,
  stoppedRole: REF_PICKER_STOPPED_ROLE,
};
