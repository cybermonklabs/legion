import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import type { CaseRow } from "./build-corpus";
import { CorpusError, listOf, numberOf, recordOf, stringOf } from "./corpus-io";
import type { DispatchEvent } from "./dispatch-read";
import { askTimeOf, pickedRoot, RESERVE_LIST_CUT, RESERVE_TREE_CUT } from "./reserve";

/**
 * The issue trees the claim's draw groups by, under item 6 of the reserve's version 2
 * (dispatch://INFRA/artifact/infra-2897-final-test-reserve-md, "The rule").
 *
 * A group is every issue that any parent link the log recorded through the build's pinned event
 * joins, so an issue that was ever in a tree stays in its group after it moves. A build writes,
 * for each group that holds a case, every ask anyone opened in it (its first opening, and the
 * actor that opened it) and every change of an issue's parent with the actor that made it
 * (`trees.json`, beside `corpus.jsonl`). The claim's draw reads that file and puts each group
 * under one of item 6's outcomes (`treeOutcomeOf`).
 *
 * Nothing here reads an answer: the file names asks by id, with their issue and opening, and
 * holds no verdict.
 */

/** The name of the file a build writes the groups to, in the corpus directory. */
export const TREES_FILE = "trees.json";

/** The shape `trees.json` is written in; a reader refuses another. */
export const TREES_VERSION = 1;

/** One ask: its first opening, which item 6 (a) reads, and every actor that opened it, for (d). */
export interface TreeAsk {
  askId: string;
  issueKey: string;
  /** The id of the event of its first opening, which a parent change is ordered against. */
  eventId: number;
  openedAt: string;
  /**
   * The ids of the actors of every `ask.opened` event of this ask, the first opening's first: a
   * session id or a login. An opening with no actor adds none.
   */
  actors: string[];
}

/** One change of an issue's parent, as two whole-issue events of the log recorded it. */
export interface ParentChange {
  eventId: number;
  issueKey: string;
  from: string | null;
  to: string | null;
  at: string;
  actor: string | null;
}

/** One group: its key (its least issue key), its issues, its asks and its parent changes. */
export interface TreeGroup {
  key: string;
  issues: string[];
  asks: TreeAsk[];
  parentChanges: ParentChange[];
}

/** What `trees.json` holds. */
export interface TreesFile {
  version: number;
  throughEventId: number;
  groups: TreeGroup[];
}

/**
 * Walks the log's events in id order and keeps what item 6 needs: every parent link any
 * whole-issue event recorded, each ask's first opening, and each change of a parent.
 */
export class TreeLog {
  /** Every parent each issue was ever recorded with, so a group never loses an issue that moved. */
  private readonly links = new Map<string, Set<string>>();
  /** The parent the latest whole-issue event recorded, which a change is read against. */
  private readonly parents = new Map<string, string | null>();
  private readonly asks = new Map<string, TreeAsk>();
  private readonly changes: ParentChange[] = [];

  /** Reads one event; the walk hands them in id order. */
  note(event: DispatchEvent): void {
    if (event.type === "ask.opened") {
      this.noteAsk(event);
      return;
    }
    // The same events `noteIssueParent` (`reserve.ts`) reads: a whole issue with its `parent`.
    if (!event.type.startsWith("issue.")) return;
    const key = event.payload.key;
    if (typeof key !== "string" || !Object.hasOwn(event.payload, "parent")) return;
    const value = event.payload.parent;
    const parent = typeof value === "string" && value !== "" ? value : null;
    if (this.parents.has(key) && this.parents.get(key) !== parent) {
      this.changes.push({
        eventId: event.id,
        issueKey: key,
        from: this.parents.get(key) ?? null,
        to: parent,
        at: event.created_at,
        actor: event.actor?.id ?? null,
      });
    }
    this.parents.set(key, parent);
    if (!this.links.has(key)) this.links.set(key, new Set());
    if (parent !== null) this.links.get(key)?.add(parent);
  }

  private noteAsk(event: DispatchEvent): void {
    const askId = event.payload.id;
    if (typeof askId !== "string") return;
    const actor = event.actor?.id ?? null;
    const known = this.asks.get(askId);
    if (known !== undefined) {
      // A reopening keeps the first opening's time and issue, and adds its actor for item 6 (d).
      if (actor !== null && !known.actors.includes(actor)) known.actors.push(actor);
      return;
    }
    const own = event.payload.issue_key;
    const issueKey = typeof own === "string" ? own : event.issue_key;
    // An ask on no issue is in no tree: the reserve never holds it, so no group needs it.
    if (issueKey === null) return;
    const opened = event.payload.created_at;
    this.asks.set(askId, {
      askId,
      issueKey,
      eventId: event.id,
      openedAt: typeof opened === "string" ? opened : event.created_at,
      actors: actor === null ? [] : [actor],
    });
  }

  /**
   * The groups that hold any of `issueKeys`, each with every issue, ask and parent change in it,
   * sorted so the same walk writes the same bytes.
   */
  groupsOf(issueKeys: readonly string[]): TreeGroup[] {
    const leader = new Map<string, string>();
    const find = (key: string): string => {
      let at = key;
      for (;;) {
        const up = leader.get(at) ?? at;
        if (up === at) break;
        // Halving the path keeps each later find short.
        leader.set(at, leader.get(up) ?? up);
        at = up;
      }
      return at;
    };
    const unite = (one: string, other: string): void => {
      const a = find(one);
      const b = find(other);
      if (a === b) return;
      // The least key leads, so a group's key is its least issue key whatever the order.
      if (a < b) leader.set(b, a);
      else leader.set(a, b);
    };
    for (const [child, parents] of this.links) {
      for (const parent of parents) unite(child, parent);
    }
    const wanted = new Set<string>();
    for (const key of issueKeys) wanted.add(find(key));
    const groups = new Map<string, TreeGroup>();
    const groupOf = (issueKey: string): TreeGroup | null => {
      const key = find(issueKey);
      if (!wanted.has(key)) return null;
      let group = groups.get(key);
      if (group === undefined) {
        group = { key, issues: [], asks: [], parentChanges: [] };
        groups.set(key, group);
      }
      return group;
    };
    const issues = new Set<string>(issueKeys);
    for (const [child, parents] of this.links) {
      issues.add(child);
      for (const parent of parents) issues.add(parent);
    }
    for (const ask of this.asks.values()) issues.add(ask.issueKey);
    for (const issue of issues) groupOf(issue)?.issues.push(issue);
    for (const ask of this.asks.values()) groupOf(ask.issueKey)?.asks.push(ask);
    for (const change of this.changes) groupOf(change.issueKey)?.parentChanges.push(change);
    const ordered = [...groups.values()].sort((one, other) => (one.key < other.key ? -1 : 1));
    for (const group of ordered) {
      group.issues.sort();
      group.asks.sort((one, other) => one.eventId - other.eventId);
      group.parentChanges.sort((one, other) => one.eventId - other.eventId);
    }
    return ordered;
  }

  /**
   * Version 3's list of the reserve, from what this walk has read: every issue of each group that
   * holds an ask, except the groups version 2 holds whole, in which every ask opened at or after
   * version 2's cut on a root its rule picks (`askRoots`, each ask's root at its first opening).
   * The keys come sorted, so the same walk gives the same list and the same sha256.
   */
  reserveListOf(askRoots: ReadonlyMap<string, string | null>): string[] {
    const cut = Date.parse(RESERVE_TREE_CUT);
    const keys: string[] = [];
    for (const group of this.groupsOf([...this.asks.values()].map((ask) => ask.issueKey))) {
      const heldWhole = group.asks.every((ask) => {
        const root = askRoots.get(ask.askId);
        return Date.parse(ask.openedAt) >= cut && typeof root === "string" && pickedRoot(root);
      });
      if (!heldWhole) keys.push(...group.issues);
    }
    return keys.sort();
  }
}

/** Why a group may or may not give the final test its cases (item 6). */
export type TreeOutcome =
  /** (a) An ask of anyone opened in it before the cut: it gives no case to any slice. */
  | "ask-before-cut"
  /** (b) A case the code does not hold, or whose ask's root at its opening its time's rule did not hold. */
  | "not-held-at-opening"
  /** (c) One of its asks is in a scorer's record of what it read. */
  | "read-by-scorer"
  /** (d) A session of the claim's round opened one of its asks. */
  | "asked-by-round"
  /** It may give the final test its cases. */
  | "eligible";

/** The outcomes, in the order item 6 checks them and every count of them is printed. */
export const TREE_OUTCOMES: readonly TreeOutcome[] = [
  "ask-before-cut",
  "not-held-at-opening",
  "read-by-scorer",
  "asked-by-round",
  "eligible",
];

/** What the claim's draw checks a group against, beside the group itself. */
export interface TreeChecks {
  /** Whether the reserve holds a case (`reserveOf` under the registered read). */
  holds: (row: CaseRow) => boolean;
  /** Every ask id any scorer's record names (`--scorer-records`). */
  readAskIds: ReadonlySet<string>;
  /** The ids of the sessions of the round that makes the claim (`--round-sessions`). */
  roundSessions: ReadonlySet<string>;
}

/**
 * Item 6's outcome for one group, given every case of the corpus in it: (a) before (b), (b)
 * before (c), (c) before (d), so a group carries the first rule it fails. Under (b) a case is
 * taken only when the reserve holds it and its ask's root at its first opening is one the rule
 * of its time held: version 2's hash picks it, for an ask before version 3's cut, or it is not in
 * version 3's list, for an ask from that cut. So a tree that joined a held tree after its answers
 * were known gives none.
 */
export function treeOutcomeOf(
  group: TreeGroup,
  cases: readonly CaseRow[],
  checks: TreeChecks,
  cut: string = RESERVE_TREE_CUT
): TreeOutcome {
  const cutTime = Date.parse(cut);
  if (group.asks.some((ask) => !(Date.parse(ask.openedAt) >= cutTime))) return "ask-before-cut";
  const opened = new Set(group.asks.map((ask) => ask.askId));
  const listCutTime = Date.parse(RESERVE_LIST_CUT);
  const allHeldAtOpening = cases.every(
    (row) =>
      checks.holds(row) &&
      typeof row.askTreeRoot === "string" &&
      (askTimeOf(row) >= listCutTime ? row.askRootListed === false : pickedRoot(row.askTreeRoot)) &&
      opened.has(row.askId)
  );
  if (cases.length === 0 || !allHeldAtOpening) return "not-held-at-opening";
  if (group.asks.some((ask) => checks.readAskIds.has(ask.askId.toLowerCase()))) {
    return "read-by-scorer";
  }
  if (
    group.asks.some((ask) =>
      ask.actors.some((actor) => checks.roundSessions.has(actor.toLowerCase()))
    )
  ) {
    return "asked-by-round";
  }
  return "eligible";
}

/**
 * The parent changes item 6 asks the claim to list: those the log recorded after the first ask of
 * the group opened, in the log's own order, since a change before any ask could not follow an
 * answer.
 */
export function changesAfterAskOf(group: TreeGroup): ParentChange[] {
  if (group.asks.length === 0) return [];
  const opened = group.asks.reduce((least, ask) => Math.min(least, ask.eventId), Infinity);
  return group.parentChanges.filter((change) => change.eventId > opened);
}

/**
 * The Jaccard similarity of two cases' word 5-grams at or above which the two are near-duplicates
 * and their groups join (dispatch://INFRA-3189 v8, "Grouping"). The reserve's item 6 has the claim
 * group near-duplicate requests as that method says.
 */
export const NEAR_DUPLICATE_JACCARD = 0.9;

/** How many words one shingle of a case's evidence holds: the method's word 5-grams. */
export const SHINGLE_WORDS = 5;

/**
 * The word 5-grams of a case's evidence, which near-duplicate grouping compares. The text is
 * lower-cased and cut into runs of letters and digits; every five consecutive runs are one
 * shingle. Each shingle is kept as the first six bytes of its sha256, a whole number below 2^48,
 * so the sets of a whole corpus fit in memory and hold none of its text. Two different shingles
 * share a number with a chance near 2^-48 for each pair of them, so the corpus's millions of
 * shingles may hold a few such pairs; each adds one shared shingle to every comparison of two
 * cases that hold one of each. A text of fewer than five words has no 5-gram, so this keeps its
 * whole run as one shingle, and two such texts join only when they are the same words, barring
 * such a shared number; a text with no word has none and joins nothing. Sorted ascending, each
 * number once.
 */
export function shinglesOf(text: string): number[] {
  const words = text.toLowerCase().match(/[\p{L}\p{N}]+/gu) ?? [];
  const runs: string[] = [];
  if (words.length > 0 && words.length < SHINGLE_WORDS) runs.push(words.join(" "));
  for (let at = 0; at + SHINGLE_WORDS <= words.length; at += 1) {
    runs.push(words.slice(at, at + SHINGLE_WORDS).join(" "));
  }
  const numbers = new Set<number>();
  for (const run of runs) {
    numbers.add(createHash("sha256").update(run, "utf8").digest().readUIntBE(0, 6));
  }
  return [...numbers].sort((one, other) => one - other);
}

/** The Jaccard similarity of two sorted sets of shingles: what they share over what either holds. */
export function jaccardOf(one: readonly number[], other: readonly number[]): number {
  let shared = 0;
  let at = 0;
  let next = 0;
  while (at < one.length && next < other.length) {
    const here = one[at] as number;
    const there = other[next] as number;
    if (here === there) {
      shared += 1;
      at += 1;
      next += 1;
    } else if (here < there) at += 1;
    else next += 1;
  }
  const either = one.length + other.length - shared;
  return either === 0 ? 0 : shared / either;
}

/** Two cases whose evidence is near-duplicate, the lesser case id first, and their similarity. */
export interface NearDuplicatePair {
  cases: [string, string];
  jaccard: number;
}

/**
 * Every pair of cases whose shingles have a Jaccard similarity of `threshold` or more and that
 * `apart` keeps, in the order of the cases' sizes then ids. A case with no shingle is in no pair.
 * `apart` lets a caller drop the pairs it has no use for before they are held, which keeps a
 * corpus of many copies of one case from holding every pair of them.
 */
export function nearDuplicatesOf(
  shingles: ReadonlyMap<string, readonly number[]>,
  apart: (one: string, other: string) => boolean = () => true,
  threshold: number = NEAR_DUPLICATE_JACCARD
): NearDuplicatePair[] {
  const entries = [...shingles.entries()]
    .filter(([, set]) => set.length > 0)
    .sort(
      ([oneId, one], [otherId, other]) =>
        one.length - other.length || (oneId < otherId ? -1 : oneId > otherId ? 1 : 0)
    );
  const pairs: NearDuplicatePair[] = [];
  for (let at = 0; at < entries.length; at += 1) {
    const [id, set] = entries[at] as [string, readonly number[]];
    for (let next = at + 1; next < entries.length; next += 1) {
      const [otherId, other] = entries[next] as [string, readonly number[]];
      // Two sets share at most the smaller's size over the larger's, and the sizes only grow
      // from here on, so no later case can reach the threshold either. The bound is the same
      // division `jaccardOf` makes for a set inside another, so rounding cannot drop a pair.
      if (set.length / other.length < threshold) break;
      if (!apart(id, otherId)) continue;
      const jaccard = jaccardOf(set, other);
      if (jaccard >= threshold) {
        pairs.push({ cases: id < otherId ? [id, otherId] : [otherId, id], jaccard });
      }
    }
  }
  return pairs;
}

/** A near-duplicate pair that joined two groups: its cases, their groups before it, its similarity. */
export interface NearDuplicateJoin extends NearDuplicatePair {
  groups: [string, string];
}

/** How the claim's draw groups the cases, and each group's outcome under item 6. */
export interface TreeGrouping {
  /**
   * The group a case is in: its tree, `tree:<key>`, or `ask:<id>` for a case on no issue, joined
   * with every group a near-duplicate case links it to; a joined group goes by its least key.
   */
  groupOf: (row: CaseRow) => string;
  /** Each group's outcome, read over every case of the corpus in it, not only the drawn ones. */
  outcomes: ReadonlyMap<string, TreeOutcome>;
  /** Each group's issues, asks and parent changes, every tree a join brought in included. */
  groups: ReadonlyMap<string, TreeGroup>;
  /** Every near-duplicate pair whose cases were in two groups, by case ids. */
  joins: NearDuplicateJoin[];
}

/**
 * Groups every case of a corpus by the trees its build wrote, joins the groups of every two cases
 * whose evidence is near-duplicate (`shingles`, by case id; each connected component is one group),
 * and puts each group under item 6, read over all its trees' asks and all its cases.
 *
 * A case on no issue is in no tree and the reserve never holds it; item 6 (a) reads its ask's own
 * opening, and an opening the corpus does not record counts as before the cut. A case whose issue
 * the trees do not name means the trees were written by another build, and a case with no
 * shingles is one whose near-duplicates cannot be found: each stops the claim.
 */
export function treeGroupingOf(
  rows: readonly CaseRow[],
  trees: TreesFile,
  checks: TreeChecks,
  shingles: ReadonlyMap<string, readonly number[]>,
  cut: string = RESERVE_TREE_CUT
): TreeGrouping {
  const byIssue = new Map<string, TreeGroup>();
  for (const group of trees.groups) {
    for (const issue of group.issues) byIssue.set(issue, group);
  }
  const treeOf = new Map<string, TreeGroup>();
  const ownGroupOf = (row: CaseRow): string => {
    if (row.issueKey === null) return `ask:${row.askId}`;
    const group = byIssue.get(row.issueKey);
    if (group === undefined) {
      throw new CorpusError(
        `the corpus's trees name no tree holding ${row.issueKey}, the issue of ${row.caseId}: ` +
          "they were written by another build"
      );
    }
    treeOf.set(`tree:${group.key}`, group);
    return `tree:${group.key}`;
  };
  const own = new Map<string, string>();
  const evidence = new Map<string, readonly number[]>();
  for (const row of rows) {
    const set = shingles.get(row.caseId);
    if (set === undefined) {
      throw new CorpusError(
        `the claim's draw holds no evidence of ${row.caseId}, so it cannot join that case's ` +
          'near-duplicates (dispatch://INFRA-3189 v8, "Grouping")'
      );
    }
    own.set(row.caseId, ownGroupOf(row));
    evidence.set(row.caseId, set);
  }
  const leader = new Map<string, string>();
  const find = (key: string): string => {
    let at = key;
    for (;;) {
      const up = leader.get(at) ?? at;
      if (up === at) return at;
      leader.set(at, leader.get(up) ?? up);
      at = up;
    }
  };
  const joins: NearDuplicateJoin[] = [];
  // A pair inside one group changes nothing, so only pairs of two groups are kept.
  for (const pair of nearDuplicatesOf(evidence, (one, other) => own.get(one) !== own.get(other))) {
    const one = own.get(pair.cases[0]) as string;
    const other = own.get(pair.cases[1]) as string;
    joins.push({ ...pair, groups: [one, other] });
    const a = find(one);
    const b = find(other);
    // The least key leads, so a joined group's key is its least whatever the order of the joins.
    if (a < b) leader.set(b, a);
    else if (b < a) leader.set(a, b);
  }
  joins.sort((one, other) => {
    const [a, b] = [one.cases.join(" "), other.cases.join(" ")];
    return a < b ? -1 : a > b ? 1 : 0;
  });
  const groupOf = (row: CaseRow): string => find(own.get(row.caseId) ?? ownGroupOf(row));
  const casesOf = new Map<string, CaseRow[]>();
  const membersOf = new Map<string, Set<string>>();
  for (const row of rows) {
    const key = groupOf(row);
    const cases = casesOf.get(key);
    if (cases === undefined) casesOf.set(key, [row]);
    else cases.push(row);
    const members = membersOf.get(key) ?? new Set<string>();
    members.add(own.get(row.caseId) as string);
    membersOf.set(key, members);
  }
  const groups = new Map<string, TreeGroup>();
  const outcomes = new Map<string, TreeOutcome>();
  const cutTime = Date.parse(cut);
  for (const [key, cases] of casesOf) {
    const members = [...(membersOf.get(key) ?? [])].sort();
    const memberTrees = members.flatMap((member) => treeOf.get(member) ?? []);
    const group: TreeGroup =
      members.length === 1 && memberTrees.length === 1
        ? (memberTrees[0] as TreeGroup)
        : {
            key: memberTrees[0]?.key ?? "",
            issues: [...new Set(memberTrees.flatMap((tree) => tree.issues))].sort(),
            asks: memberTrees
              .flatMap((tree) => tree.asks)
              .sort((one, other) => one.eventId - other.eventId),
            parentChanges: memberTrees
              .flatMap((tree) => tree.parentChanges)
              .sort((one, other) => one.eventId - other.eventId),
          };
    groups.set(key, group);
    const issuelessBeforeCut = cases.some(
      (row) => row.issueKey === null && !(Date.parse(row.askCreatedAt ?? "") >= cutTime)
    );
    outcomes.set(
      key,
      issuelessBeforeCut ? "ask-before-cut" : treeOutcomeOf(group, cases, checks, cut)
    );
  }
  return { groupOf, outcomes, groups, joins };
}

/** The ids (UUIDs) a text names, in lower case: what a scorer's record or a session list holds. */
export function idsIn(text: string): Set<string> {
  const ids = new Set<string>();
  for (const match of text.matchAll(
    /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi
  )) {
    ids.add(match[0].toLowerCase());
  }
  return ids;
}

/**
 * Every id the files name, read whole. A file that cannot be read stops the claim, and so does a
 * file that names no id: item 6 (c) or (d) would then check nothing against it, which is no check.
 */
export async function idsInFiles(paths: readonly string[]): Promise<Set<string>> {
  const ids = new Set<string>();
  for (const path of paths) {
    let text: string;
    try {
      text = await readFile(path, "utf8");
    } catch {
      throw new CorpusError(`${path} could not be read, so the claim cannot check it`);
    }
    const named = idsIn(text);
    if (named.size === 0) {
      throw new CorpusError(`${path} names no id, so the claim would check nothing against it`);
    }
    for (const id of named) ids.add(id);
  }
  return ids;
}

function treeAskOf(value: unknown): TreeAsk | null {
  const record = recordOf(value);
  const askId = stringOf(record?.askId);
  const issueKey = stringOf(record?.issueKey);
  const eventId = numberOf(record?.eventId);
  const openedAt = stringOf(record?.openedAt);
  const actors = listOf(record?.actors).map(stringOf);
  if (askId === null || issueKey === null || eventId === null || openedAt === null) return null;
  // An actor that is no string would be an opening (d) cannot check: the ask is unreadable.
  if (!Array.isArray(record?.actors) || actors.some((actor) => actor === null)) return null;
  return { askId, issueKey, eventId, openedAt, actors: actors as string[] };
}

function changeOf(value: unknown): ParentChange | null {
  const record = recordOf(value);
  const eventId = numberOf(record?.eventId);
  const issueKey = stringOf(record?.issueKey);
  const at = stringOf(record?.at);
  if (eventId === null || issueKey === null || at === null) return null;
  return {
    eventId,
    issueKey,
    from: stringOf(record?.from),
    to: stringOf(record?.to),
    at,
    actor: stringOf(record?.actor),
  };
}

/**
 * The groups a corpus's build wrote, with the sha256 of the bytes they were read from, which a
 * claim draw records so the next stage of its run reads the same file. A corpus with no
 * `trees.json`, or one of another shape, was built before the build wrote them, and the claim's
 * draw cannot run over it.
 */
export async function readTrees(
  corpusDirectory: string
): Promise<{ trees: TreesFile; sha256: string }> {
  const path = join(corpusDirectory, TREES_FILE);
  let parsed: unknown;
  let sha256: string;
  try {
    const bytes = await readFile(path);
    sha256 = createHash("sha256").update(bytes).digest("hex");
    parsed = JSON.parse(bytes.toString("utf8"));
  } catch {
    throw new CorpusError(
      `${path} could not be read: the corpus was built before build-corpus.ts wrote its trees, ` +
        "which the claim's draw groups by (the reserve's item 6); build it again"
    );
  }
  const record = recordOf(parsed);
  const throughEventId = numberOf(record?.throughEventId);
  if (record === null || record.version !== TREES_VERSION || throughEventId === null) {
    throw new CorpusError(`${path} is not trees.json version ${TREES_VERSION}`);
  }
  const groups: TreeGroup[] = [];
  for (const entry of listOf(record.groups)) {
    const group = recordOf(entry);
    const key = stringOf(group?.key);
    if (group === null || key === null) throw new CorpusError(`${path} holds a group with no key`);
    const asks = listOf(group.asks).map(treeAskOf);
    const parentChanges = listOf(group.parentChanges).map(changeOf);
    const issues = listOf(group.issues).map(stringOf);
    if (
      asks.some((ask) => ask === null) ||
      parentChanges.some((change) => change === null) ||
      issues.some((issue) => issue === null)
    ) {
      throw new CorpusError(`${path} holds a group, ${key}, with an entry it cannot read`);
    }
    groups.push({
      key,
      issues: issues as string[],
      asks: asks as TreeAsk[],
      parentChanges: parentChanges as ParentChange[],
    });
  }
  return { trees: { version: TREES_VERSION, throughEventId, groups }, sha256 };
}
