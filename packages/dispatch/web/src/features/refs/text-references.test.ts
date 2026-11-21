import { expect, test } from "bun:test";
import { DISPATCH_TEXT_REFERENCES, isIdPrefix } from "@legion/contracts";

import { referenceReads, referenceReadsSince } from "./reference-reads";
import { readTextReferences } from "./text-references";

const origin = "https://dispatch.test";

/** The references a reading names, in order of first appearance, as a row's `refs` lists them. */
function cited(body: string, facts: Parameters<typeof readTextReferences>[1]): string[] {
  const seen = new Set<string>();
  for (const found of readTextReferences(body, facts).references) {
    if (found.reference !== undefined) {
      seen.add(found.reference);
    }
  }
  return [...seen];
}

// The reader's half of the shared text table: every row, the ones a bare key, a `#id` or an id
// prefix writes included, read against what the row says exists. The Go reader `text.Mentions`
// walks the same rows, so neither where a reference ends nor what it names can change on one
// side alone.
test.each(
  DISPATCH_TEXT_REFERENCES.map((row) => [row.body, row.origin ?? origin, row] as const)
)("%s cites, read against %s, its row's references", (body, server, row) => {
  expect(
    cited(body, {
      issues: row.issues,
      items: row.items,
      origin: server,
      questions: row.questions,
    })
  ).toEqual([...row.refs]);
});

// What a caller that has asked the server nothing would have to ask: the distinct keys, whether
// a `#id` is there, and each reference whose id is written short. A hook starts exactly these.
test("a reading answers what it would have to ask before it has asked anything", () => {
  const { needs, references } = readTextReferences(
    "Both CORE-1 and ACME-12 are blocked; CORE-1 again, see #q2 and dispatch://CORE-1/ask/2a7f1c90.",
    { origin }
  );

  expect(needs).toEqual({
    keys: ["CORE-1", "ACME-12"],
    prefixes: ["dispatch://CORE-1/ask/2a7f1c90"],
    questions: true,
  });
  // Nothing is resolved yet, and every candidate says so.
  expect(references.map(({ form, text, unresolved }) => ({ form, text, unresolved }))).toEqual([
    { form: "key", text: "CORE-1", unresolved: true },
    { form: "key", text: "ACME-12", unresolved: true },
    { form: "key", text: "CORE-1", unresolved: true },
    { form: "question", text: "#q2", unresolved: true },
    { form: "address", text: "dispatch://CORE-1/ask/2a7f1c90", unresolved: true },
  ]);
});

test("a text with nothing to resolve asks nothing", () => {
  const { needs, references } = readTextReferences("Tied to dispatch://CORE-1 here.", { origin });

  expect(needs).toEqual({ keys: [], prefixes: [], questions: false });
  expect(references.map((found) => found.reference)).toEqual(["dispatch://CORE-1"]);
  expect(references[0].unresolved).toBe(false);
});

// The id prefix pattern, which the server spells `text.IsIDPrefix`: a hex digit and seven or
// more hex digits and hyphens, read without case, and never a whole uuid.
test.each([
  ["2a7f1c90", true],
  ["2A7F1C90", true],
  ["2a7f1c90-1f4e", true],
  ["2a7f1c9", false],
  ["xa7f1c90", false],
  ["2a7f1c9g", false],
  ["2a7f1c90-1f4e-4a2b-9c3d-5e6f70819a2b", false],
  ["2a7f1c901f4e4a2b9c3d5e6f70819a2b", false],
] as const)("%s is an id prefix: %p", (value, expected) => {
  expect(isIdPrefix(value)).toBe(expected);
});

// A reading hands each address it finds to `readAddress` as the author wrote it and, when it
// ends in `_` or `-`, once more without them (`trimmedMention`), and `readAddress` hands it to
// `referenceRouteFromHref` once: two passes over the addresses of a body at most, counted rather
// than timed (`reference-reads.ts`). The last address holds a 65,536-character run of `-`
// before a letter, the text `trimmedMention`'s end-anchored `/[_-]+$/` tried from every position
// of the run (1.7 s for a run of 32,768 in a 2-CPU pod, four times that for each doubling); its
// walk back reads only the letter that stops it. The pattern read nothing the count could see,
// so that version fails the call count below. A reader that handed the body to `readAddress`
// again for each address reads about the square of it.
test("a reading hands each address to readAddress at most twice and trims its tail walking back", () => {
  const trailed = `${origin}/issues/CORE-1?x=${"-".repeat(64)}`;
  const long = `${origin}/issues/CORE-2?x=${"-".repeat(1 << 16)}y`;
  const count = 1024;
  const body = `${Array.from({ length: count }, () => trailed).join(" ")} ${long}`;
  const before = referenceReads();
  const { references } = readTextReferences(body, { origin });
  const read = referenceReadsSince(before);
  expect(references.map((found) => [found.reference, found.named])).toEqual([
    ...Array.from({ length: count }, () => ["dispatch://CORE-1", trailed.length - 64]),
    ["dispatch://CORE-2", undefined],
  ]);
  // Each trailed address is read as written and without its run of `-`; the long one, which
  // ends in a letter, only as written.
  expect(read.readAddress.calls).toBe(2 * count + 1);
  expect(read.readAddress.characters).toBeLessThanOrEqual(2 * body.length);
  expect(read.referenceRouteFromHref.calls).toBe(2 * count + 1);
  expect(read.referenceRouteFromHref.characters).toBeLessThanOrEqual(2 * body.length);
  // The walk back reads each trailed address's run and the `=` that stops it, and the long
  // address's last letter.
  expect(read.trimmedMention.calls).toBe(count + 1);
  expect(read.trimmedMention.characters).toBe(count * 65 + 1);
});
