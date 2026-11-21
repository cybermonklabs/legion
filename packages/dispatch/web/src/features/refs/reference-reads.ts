/**
 * How much each reader of the references in a body has read since the page loaded: how many
 * times it was called and how many characters it read. Every comment body and ask question is
 * read as it renders, and one body holds up to 1 MiB, so a reader that read a long body more
 * than a few times over would hang the reader's tab. A test holds each reader of a body to a
 * number of passes over it by counting this work, where a clock would read the machine.
 *
 * - `trimReference` and `trimmedMention` count the characters their loops read, so a trim that
 *   rescanned a run from every position in it reads about the square of its length here, and one
 *   handed to a regular expression reads nothing.
 * - `readAddress`, `referenceRouteFromHref` and `githubReference` count the characters of each
 *   value they are handed. Within one call each reads that value a fixed number of times over,
 *   through loops, `URL`, percent-decoding and regular expressions; what holds each pattern to
 *   a pass linear in the value is recheck's reading of it (`reference-patterns.test.ts`), since
 *   JavaScriptCore counts no step a regular expression takes. So a caller that handed a reader
 *   the body again for every reference in it reads about the square of the body here.
 */
export const REFERENCE_READERS = [
  "trimReference",
  "trimmedMention",
  "readAddress",
  "referenceRouteFromHref",
  "githubReference",
] as const;

export type ReferenceReader = (typeof REFERENCE_READERS)[number];

/** One reader's calls and the characters they read. */
export interface ReaderReads {
  readonly calls: number;
  readonly characters: number;
}

/** No reader has read anything. */
function noReads(): Record<ReferenceReader, { calls: number; characters: number }> {
  return {
    githubReference: { calls: 0, characters: 0 },
    readAddress: { calls: 0, characters: 0 },
    referenceRouteFromHref: { calls: 0, characters: 0 },
    trimmedMention: { calls: 0, characters: 0 },
    trimReference: { calls: 0, characters: 0 },
  };
}

const reads = noReads();

/** Counts one call of `reader` that read `characters` characters. */
export function countReferenceRead(reader: ReferenceReader, characters: number): void {
  reads[reader].calls += 1;
  reads[reader].characters += characters;
}

/** Each reader's calls and characters since the page loaded. */
export function referenceReads(): Readonly<Record<ReferenceReader, ReaderReads>> {
  const copy = noReads();
  for (const reader of REFERENCE_READERS) {
    copy[reader].calls = reads[reader].calls;
    copy[reader].characters = reads[reader].characters;
  }
  return copy;
}

/** Each reader's calls and characters since `before`, a reading of `referenceReads`. */
export function referenceReadsSince(
  before: Readonly<Record<ReferenceReader, ReaderReads>>
): Readonly<Record<ReferenceReader, ReaderReads>> {
  const since = noReads();
  for (const reader of REFERENCE_READERS) {
    since[reader].calls = reads[reader].calls - before[reader].calls;
    since[reader].characters = reads[reader].characters - before[reader].characters;
  }
  return since;
}
