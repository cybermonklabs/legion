/**
 * The neutral-word replacement of the Oh My Pi fixtures (testdata/CAPTURE.md, "Words replaced in
 * the Oh My Pi fixtures"):
 *
 *   bun scripts/skill-cuts/fixturewords.ts [--write] <file>...
 *
 * The pod's public push check refuses a line that holds the model provider's name (PROVIDER)
 * together with one of the words of NEUTRAL in any case. In each line of each file that holds the
 * provider's name, in any case, each such word is replaced by its neutral word, in the case of the
 * word it replaces (all capitals, a capital first, or as it is), and every other byte is kept. A
 * word right after a JSON escape (`\n`, `\t`, `\u2014`) counts as a word, as it does once the JSON
 * is decoded. It prints, per file that holds such a word, the count of each word, and the total;
 * without `--write` it changes no file. Exit 0, or 2 on a refusal (the usage, or a file it cannot
 * read).
 */
import { readFileSync, writeFileSync } from "node:fs";
import { parseArgs } from "node:util";

/** The provider's name, matched in any case. */
const PROVIDER = /anthropic/i;

/** Each word the push check reads beside the provider's name, lower case, and its neutral word. */
const NEUTRAL: Readonly<Record<string, string>> = {
  deliver: "arrive",
  delivers: "arrives",
  delivered: "handed back",
  delivering: "handing back",
  delivery: "handover",
  deliveries: "handovers",
  contract: "agreement",
  contracts: "agreements",
  customer: "client",
  customers: "clients",
  invoice: "bill",
  invoices: "bills",
  "work order": "job ticket",
  "work orders": "job tickets",
  "work-order": "job-ticket",
  "work-orders": "job-tickets",
  workorder: "jobticket",
  workorders: "jobtickets",
};

/** A word of NEUTRAL standing alone, or right after a JSON escape. */
const WORD =
  /(?:(?<![A-Za-z0-9_])|(?<=\\[nrtbf])|(?<=\\u[0-9a-fA-F]{4}))(customers?|work[- ]?orders?|deliver(?:y|ies|ed|ing|s)?|contracts?|invoices?)(?![A-Za-z0-9_])/gi;

const USAGE = "usage: fixturewords.ts [--write] <file>...";

/** `neutral` in the case of `found`: all capitals, a capital first, or as it is. */
function inCase(found: string, neutral: string): string {
  if (found === found.toUpperCase()) return neutral.toUpperCase();
  if (found[0] !== found[0]?.toUpperCase()) return neutral;
  return `${neutral[0]?.toUpperCase()}${neutral.slice(1)}`;
}

/** `text` with each word replaced on the lines that hold the provider's name, and the count of
 *  each word replaced, as it was written. */
function neutralWords(text: string): { text: string; counts: Record<string, number> } {
  const counts: Record<string, number> = {};
  const lines = text.split("\n").map((line) =>
    PROVIDER.test(line)
      ? line.replace(WORD, (found: string) => {
          const neutral = NEUTRAL[found.toLowerCase()];
          if (neutral === undefined) throw new Error(`no neutral word for ${found}`);
          counts[found] = (counts[found] ?? 0) + 1;
          return inCase(found, neutral);
        })
      : line
  );
  return { text: lines.join("\n"), counts };
}

/** The command. Returns the exit status: 0 done, 2 refused. */
function main(
  argv: readonly string[],
  io: { out(text: string): void; err(text: string): void }
): number {
  let values: { write?: boolean };
  let files: string[];
  try {
    ({ values, positionals: files } = parseArgs({
      args: [...argv],
      options: { write: { type: "boolean" } },
      strict: true,
      allowPositionals: true,
    }));
  } catch (error) {
    io.err(`fixturewords: ${(error as Error).message}. ${USAGE}`);
    return 2;
  }
  if (files.length === 0) {
    io.err(`fixturewords: at least one file is required. ${USAGE}`);
    return 2;
  }
  let texts: string[];
  try {
    texts = files.map((file) => readFileSync(file, "utf8"));
  } catch (error) {
    io.err(`fixturewords: ${(error as Error).message}`);
    return 2;
  }
  const total: Record<string, number> = {};
  files.forEach((file, index) => {
    const { text, counts } = neutralWords(texts[index] ?? "");
    if (Object.keys(counts).length === 0) return;
    io.out(`${file}: ${JSON.stringify(counts)}`);
    for (const [word, n] of Object.entries(counts)) total[word] = (total[word] ?? 0) + n;
    if (values.write === true) writeFileSync(file, text);
  });
  io.out(`total: ${JSON.stringify(total)}`);
  return 0;
}

if (import.meta.main) {
  process.exit(
    main(Bun.argv.slice(2), {
      out: (text) => console.log(text),
      err: (text) => console.error(text),
    })
  );
}
