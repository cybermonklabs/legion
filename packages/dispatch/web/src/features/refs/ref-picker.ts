import type { SearchResultKind } from "../../api/types";

/**
 * The reference picker's rows, their order and the text a pick inserts: one module, so the three
 * hosts that draw it - the shared text field, the document body and the agent live conversation -
 * cannot drift into three pickers. It owns no textarea and no ProseMirror: a host reads the text
 * before its caret, draws the rows this module groups, passes each key to it, and applies the
 * insertion it hands back.
 *
 * `useRefPicker.ts` reads the server for the rows; `RefPickerList.tsx` draws them.
 */

/** Every kind the picker reaches, which is every kind a reference can name except an artifact of
 *  an agent's conversation, which no box writes about. A block is its holder's kind in its own
 *  right: its rows come from another read (the holder's `part_of` backlinks) and sit under a
 *  heading of their own. */
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

/** The parties: the kinds whose pick inserts a worded link rather than a bare address (R4). */
export const REF_PICKER_PARTY_KINDS = ["person", "agent", "role"] as const;

export type RefPickerPartyKind = (typeof REF_PICKER_PARTY_KINDS)[number];

export function isPartyKind(kind: RefPickerKind): kind is RefPickerPartyKind {
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

/** One row of the picker. `id` is unique within one open: it is the row's React key and the
 *  value a host puts in `aria-activedescendant`. */
export interface RefPickerRow {
  readonly id: string;
  readonly kind: RefPickerKind;
  /** The row's own words - an issue's title, a party's name, a block's first words - which the
   *  browser-side filter matches and the list draws. */
  readonly label: string;
  /** The line under the label: where the row is, or what it is. Empty for a row with nothing to
   *  add. */
  readonly detail: string;
  /** The `dispatch://` address the row names. */
  readonly reference: string;
  /** The address whose `part_of` backlinks are this row's blocks, for a node that holds any; a
   *  host opens them with `openParts`. */
  readonly parts?: string;
}

/** One kind's rows, under that kind's heading. */
export interface RefPickerGroup {
  readonly kind: RefPickerKind;
  readonly heading: string;
  readonly rows: readonly RefPickerRow[];
}

/** The rows grouped by kind, the groups in this door's order; a kind with no row has no group. */
export function groupRows(rows: readonly RefPickerRow[], door: RefPickerDoor): RefPickerGroup[] {
  return kindOrder(door).flatMap((kind) => {
    const ofKind = rows.filter((row) => row.kind === kind);
    return ofKind.length === 0 ? [] : [{ heading: REF_PICKER_HEADINGS[kind], kind, rows: ofKind }];
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

/** The door a trigger opened, where its first character sits in the text the host read, and what
 *  the reader has typed after it. */
export interface RefPickerTrigger {
  readonly door: RefPickerDoor;
  readonly start: number;
  /** Never holds a space: a space typed before a row is taken closes the picker. */
  readonly query: string;
}

/** A trigger opens only at a word boundary - the start of the text, or after a space, a line
 *  break or an opening bracket or quote - so `issue#5` opens nothing, and a `#` that starts a
 *  line is still the Markdown heading it becomes as soon as the reader types the space. */
function atWordBoundary(before: string, start: number): boolean {
  if (start === 0) return true;
  return /[\s([{"']/u.test(before.charAt(start - 1));
}

/**
 * The door open at the caret, read from the text before it: the last trigger the reader typed
 * that still stands - at a word boundary, with no space typed since. Undefined when none does,
 * which is how the picker closes on a space and how it never opens mid-word.
 */
export function readTrigger(before: string): RefPickerTrigger | undefined {
  let found: RefPickerTrigger | undefined;
  for (const { chars, door } of REF_PICKER_TRIGGERS) {
    const start = before.lastIndexOf(chars);
    if (start < 0 || (found !== undefined && start < found.start)) continue;
    if (!atWordBoundary(before, start)) continue;
    const query = before.slice(start + chars.length);
    if (!/\s/u.test(query)) found = { door, query, start };
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
}

/** A link's words cannot hold a bare bracket; a session title can. */
function linkWords(label: string): string {
  return label.replace(/[[\]]/gu, (bracket) => `\\${bracket}`);
}

/** The text a pick inserts: a party as a link whose words are its name and whose address names
 *  it (R4), so the stored text reads as the person, session or role it mentions; every other
 *  kind as the bare address, which the renderer draws by its target's own title (R8). */
export function insertionText(row: RefPickerRow): string {
  return isPartyKind(row.kind) ? `[${linkWords(row.label)}](${row.reference})` : row.reference;
}

/** The insertion a pick of `row` makes in a draft whose caret is at `caret`. */
export function insertionFor(
  row: RefPickerRow,
  trigger: RefPickerTrigger,
  caret: number
): RefPickerInsertion {
  return { end: caret, start: trigger.start, text: insertionText(row) };
}

/** `insertion` applied to a plain string, with the caret that follows it: what a `<textarea>`
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

/** The next highlighted row after a move, wrapping at either end of the list. */
export function stepRow(index: number, delta: 1 | -1, count: number): number {
  return count === 0 ? 0 : (index + delta + count) % count;
}

/** What the list is, while it is not a list of rows. A host draws the words below. */
export type RefPickerState = "rows" | "loading" | "empty" | "unreachable";

/** The words of each state the list has no rows for, in the composer's own shapes
 *  (`MentionComposer.tsx`: "Loading live agents…", "No live agents match.", "Could not load live
 *  agents — Envoy listener unreachable."). An agent list that is refilling is a list not ready
 *  yet, so it reads as loading rather than as a failure. */
export const REF_PICKER_WORDS: Record<Exclude<RefPickerState, "rows">, string> = {
  empty: "No reference matches.",
  loading: "Loading references…",
  unreachable: "Could not load references — Dispatch unreachable.",
};

/** The one row the picker shows for the kinds it cannot list yet: the search route refuses a
 *  query under two characters, so until the reader has typed two the list holds the parties and
 *  the components, and this says what the next two letters will add. */
export const REF_PICKER_INSTRUCTION =
  "Type two characters to search issues, documents, asks, comments and messages.";
