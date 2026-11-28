import { numberOf, recordOf, stringOf } from "./corpus-io";
import { microsecondsOf } from "./reserve";

/**
 * The frozen S10 reviewer's own reads of a request, mirrored in TypeScript, so the inputs the
 * claim writes for S10 (`s10-inputs.ts`) hold exactly what S10's Python code reads, read the way
 * it reads it. Each function here names the Python function it mirrors, and holds to its rules,
 * Python's truthiness among them (`falsy`): `ask_docs` (`askDocsOf`), `parent_key`
 * (`parentKeyOf`), `legion_fact` (`legionFactOf`), the order and the reads of `neighbours`
 * (`neighbourOrder`, `neighbourFault`), and the cut of a search query (`firstCharacters`).
 *
 * Nothing here reads Dispatch, the log or a file: each function is pure, over the values it is
 * handed, and holds no answer. `s10_judge.py` runs the frozen Python itself over the files the
 * inputs stage writes; these mirrors are what lets that stage know, before it writes a file,
 * which fields the Python will read and whether the log gives them.
 */

/** How much of a query S10 sends, in characters (code points, as Python counts them). */
export const QUERY_CHARACTERS = 1000;

/** How many neighbours S10's `neighbours` keeps before it stops reading candidates. */
export const NEIGHBOURS_KEPT = 8;

/** The first `count` characters of a text, as Python's `text[:count]` takes them. */
export function firstCharacters(text: string, count: number = QUERY_CHARACTERS): string {
  return Array.from(text).slice(0, count).join("");
}

/**
 * Whether Python reads a value as false, which is what its `x or y` passes over: none, false, a
 * zero, and an empty text, list or object.
 */
export function falsy(value: unknown): boolean {
  if (value === undefined || value === null || value === false || value === 0 || value === "") {
    return true;
  }
  if (Array.isArray(value)) return value.length === 0;
  const record = recordOf(value);
  return record !== null && Object.keys(record).length === 0;
}

/** One document an approval names, as S10's `ask_docs` reads it. */
export interface AskDocument {
  artifact_id: string;
  name: string;
  version: number;
}

/**
 * The documents an approval names as S10's `ask_docs` reads them, each with an `artifact_id`, a
 * `name` and a `version`, or the first place where it reads one the approval does not give.
 * `ask_docs` reads `approval.documents` whenever Python takes it for true, every entry of it,
 * and falls back to the approval's own document only where that list is absent or empty, and to
 * none where there is no approval: `ap.get("documents") or ([...] if ap else [])`. So a list
 * holding an entry that is no document, or naming no document field, gives no documents at all,
 * the place of that entry named, and never the approval's own document in their stead.
 */
export function askDocsOf(approval: unknown): { documents: AskDocument[] } | { missing: string } {
  if (falsy(approval)) return { documents: [] };
  const record = recordOf(approval);
  if (record === null) return { missing: "approval" };
  const listed = record.documents;
  if (!falsy(listed) && !Array.isArray(listed)) return { missing: "approval.documents" };
  const entries: Array<[string, unknown]> =
    Array.isArray(listed) && listed.length > 0
      ? listed.map((entry, at): [string, unknown] => [`approval.documents[${at}]`, entry])
      : [["approval", record]];
  const documents: AskDocument[] = [];
  for (const [where, entry] of entries) {
    const document = recordOf(entry);
    if (document === null) return { missing: where };
    const artifactId = stringOf(document.artifact_id);
    const name = stringOf(document.name);
    const version = numberOf(document.version);
    if (artifactId === null) return { missing: `${where}.artifact_id` };
    if (name === null) return { missing: `${where}.name` };
    if (version === null || !Number.isSafeInteger(version)) return { missing: `${where}.version` };
    documents.push({ artifact_id: artifactId, name, version });
  }
  return { documents };
}

/** S10's `legion_fact` over a request and its issue, field for field as it returns them. */
export interface LegionFact {
  holds: boolean;
  legion: boolean;
  primary: boolean;
  root: boolean;
  status: unknown;
}

/**
 * An issue's parent as S10's `parent_key` reads it, and null where it reads none: a key written
 * as text, or an object's `key`, `issue_key` or `id`, the first Python takes for true, or else
 * its `id`, whatever that holds. `undefined` where `parent_key` cannot read the value at all.
 */
export function parentKeyOf(parent: unknown): unknown {
  if (falsy(parent)) return null;
  if (typeof parent === "string") return parent;
  const record = recordOf(parent);
  if (record === null) return undefined;
  for (const field of ["key", "issue_key"]) {
    if (!falsy(record[field])) return record[field];
  }
  return record.id ?? null;
}

/**
 * S10's `legion_fact` over the request as the inputs stage writes it and its issue as of the
 * judged moment, mirrored exactly, or null where Python would stop on a value it cannot read (a
 * parent `parent_key` cannot read, labels `in` cannot search). It holds for the primary document
 * of a root issue carrying the `legion` label that waits on the backlog or in icebox: S10 decides
 * that request never-decide without its judges or its neighbours.
 */
export function legionFactOf(
  ask: Readonly<Record<string, unknown>>,
  issue: Readonly<Record<string, unknown>>
): LegionFact | null {
  const artifact = recordOf(ask.approval_artifact) ?? {};
  const approved = recordOf(ask.approval)?.artifact_id ?? null;
  const primary = !falsy(artifact.primary) || approved === (issue.primary_artifact_id ?? null);
  const parent = parentKeyOf(issue.parent);
  if (parent === undefined) return null;
  const labels = falsy(issue.labels) ? [] : issue.labels;
  let legion: boolean;
  if (Array.isArray(labels)) legion = labels.includes("legion");
  else if (typeof labels === "string") legion = labels.includes("legion");
  else if (recordOf(labels) !== null) legion = Object.hasOwn(labels as object, "legion");
  else return null;
  const root = parent === null;
  const status = issue.status ?? null;
  const holds = root && legion && primary && (status === "backlog" || status === "icebox");
  return { holds, legion, primary, root, status };
}

/** One ask among a request's search hits, as `neighbours` orders them. */
export interface NeighbourHit {
  id: string;
  rank: number | null;
}

/**
 * One candidate as `neighbourFault` reads it: held by the reserve, dropped by an event after the
 * judged moment, or its ask's fields and its issue as of that moment.
 */
export type NeighbourCandidate =
  | { held: true }
  | { dropped: string }
  | {
      ask: Readonly<Record<string, unknown>>;
      issue: { key: string; title: string | null } | null;
    };

/**
 * The ask hits of a request in the order S10's `neighbours` reads them: each ask once, at the
 * highest rank any page gave it, kept where it was first met, the title's pages before the
 * question's (`hits` in that order); then by rank, highest first, the order met breaking a tie.
 */
export function neighbourOrder(hits: readonly NeighbourHit[]): string[] {
  const best = new Map<string, number>();
  for (const hit of hits) {
    const rank = hit.rank ?? 0;
    const known = best.get(hit.id);
    if (known === undefined || known < rank) best.set(hit.id, rank);
  }
  return [...best.entries()].sort((one, other) => other[1] - one[1]).map(([id]) => id);
}

/** What `neighbourFault` reads of the judged request. */
export interface JudgedForNeighbours {
  person: string;
  issueKey: string;
  /** The request's first opening, in microseconds: S10's `opened_at`. */
  openedAt: number;
  /** The documents the request names, by artifact id. */
  documents: ReadonlySet<string>;
}

/**
 * The first field S10's `neighbours` reads of a candidate that the log cannot give as of the
 * judged moment, and the candidate, or null where it reads none such. It takes the candidates in
 * `order`, passing over the held and the dropped, and stops at its eighth neighbour, as S10 does;
 * each field is read only where the ones before it let the candidate through.
 */
export function neighbourFault(
  order: readonly string[],
  candidates: Readonly<Record<string, NeighbourCandidate>>,
  judged: JudgedForNeighbours
): { askId: string; field: string } | null {
  let kept = 0;
  for (const askId of order) {
    if (kept >= NEIGHBOURS_KEPT) break;
    const candidate = candidates[askId];
    if (candidate === undefined || "held" in candidate || "dropped" in candidate) continue;
    const fault = (field: string): { askId: string; field: string } => ({ askId, field });
    const ask = candidate.ask;
    if (typeof ask.state !== "string") return fault("state");
    if (!Object.hasOwn(ask, "answer")) return fault("answer");
    const answer = recordOf(ask.answer);
    if (ask.state !== "answered" || answer === null) continue;
    if (!Object.hasOwn(answer, "user")) return fault("answer.user");
    if (answer.user !== judged.person) continue;
    if (!Object.hasOwn(ask, "issue_key")) return fault("issue_key");
    if (ask.issue_key === judged.issueKey) continue;
    const at = microsecondsOf(stringOf(answer.at) ?? "");
    if (at === null) return fault("answer.at");
    if (at >= judged.openedAt) continue;
    if (typeof ask.kind !== "string") return fault("kind");
    if (ask.kind !== "approval") continue;
    if (!Object.hasOwn(ask, "approval")) return fault("approval");
    const documents = askDocsOf(ask.approval);
    if ("missing" in documents) return fault(documents.missing);
    if (documents.documents.some((one) => judged.documents.has(one.artifact_id))) continue;
    if (!Array.isArray(answer.selected)) return fault("answer.selected");
    if (!Object.hasOwn(answer, "text")) return fault("answer.text");
    if (typeof ask.question !== "string") return fault("question");
    if (candidate.issue !== null && candidate.issue.title === null) return fault("issue title");
    kept += 1;
  }
  return null;
}
