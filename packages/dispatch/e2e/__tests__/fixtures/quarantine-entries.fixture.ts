/** The planted controls of `e2e/__tests__/quarantine.test.ts`: quarantines the check must refuse,
 *  each for one fault. The rows they name are planted too - the check of an entry's shape reads
 *  no listing, so a row here need not exist - and every one of them is in `chromium`, a project
 *  whose rows retry on CI, so the only fault each list carries is the one its name says. */
import type { QuarantineEntry, UncheckedEntry } from "../../quarantine";

/** Eleven well-formed entries: one more than the quarantine may hold. */
export const elevenRows: readonly QuarantineEntry[] = Array.from({ length: 11 }, (_, index) => ({
  file: "planted.e2e.ts",
  project: "chromium",
  title: `a planted row ${index + 1}`,
  issue: `dispatch://INFRA-${901 + index}`,
  added: "2026-11-02",
}));

/** One entry that names no issue. */
export const withoutIssue: readonly UncheckedEntry[] = [
  {
    file: "planted.e2e.ts",
    project: "chromium",
    title: "a planted row with no owner",
    added: "2026-11-02",
  },
];

/** One entry that gives no date. */
export const withoutDate: readonly UncheckedEntry[] = [
  {
    file: "planted.e2e.ts",
    project: "chromium",
    title: "a planted row with no date",
    issue: "dispatch://INFRA-983",
  },
];

/** One entry whose issue is a key with no `dispatch://`, and whose date is no calendar day. */
export const malformed: readonly UncheckedEntry[] = [
  {
    file: "planted.e2e.ts",
    project: "chromium",
    title: "a planted row written loosely",
    issue: "INFRA-983",
    added: "2026-02-30",
  },
];

/** One fix whose commit is a short sha in upper case, and whose issue is missing. */
export const looseFix: readonly UncheckedEntry[] = [
  {
    file: "planted.e2e.ts",
    project: "chromium",
    title: "a planted row fixed loosely",
    fixed: "708CA88",
  },
];

/** One well-formed entry of a row of `search-tiers.e2e.ts`, the file `bun run e2e:embedder`
 *  runs: a row only the embedder job runs. Its title is planted; its file is the one that job's
 *  listing holds, which the test that reads this entry checks first. */
export const embedderRow: readonly QuarantineEntry[] = [
  {
    file: "search-tiers.e2e.ts",
    project: "chromium",
    title: "a planted row of the embedder job",
    issue: "dispatch://INFRA-983",
    added: "2026-11-02",
  },
];
