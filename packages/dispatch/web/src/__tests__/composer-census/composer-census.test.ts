/**
 * The composer census (INFRA-1503) held to the draft matrix: every item the census finds, of
 * every `MentionComposer` mount, has an entry in a draft matrix file's coverage list, a row of
 * that file or one of its numbered reasons; every entry names an item the census still finds; a
 * row names a test of its file; and a reason has words. So a host that adds a conditional above
 * its composer, a new mount, or a new form of the layout fails here until a matrix file answers
 * for it. The census fails closed: a mount it cannot place, or an element, a helper or a slot
 * between a mount and the routes it cannot read, fails here too, by where it is.
 *
 * The census reads the `web/src` this file is in, or the copy of it `COMPOSER_CENSUS_ROOT`
 * names, so that a change planted in a scratch copy is censused with this test unchanged. The
 * coverage lists and the matrix files' titles are always this tree's.
 */
import { expect, test } from "bun:test";
import { join } from "node:path";

import { coverage as signIn } from "../../draft-matrix.sign-in.coverage";
import { coverage as agentsRow } from "../../features/agents/draft-matrix.agents-row.coverage";
import { coverage as docked } from "../../features/conversation/draft-matrix.docked.coverage";
import { coverage as edit } from "../../features/conversation/draft-matrix.edit.coverage";
import { coverage as inlineReply } from "../../features/conversation/draft-matrix.inline-reply.coverage";
import { coverage as issuePage } from "../../features/conversation/draft-matrix.issue-page.coverage";
import { coverage as phoneThread } from "../../features/conversation/draft-matrix.phone-thread.coverage";
import { coverage as messageBox } from "../../features/inbox/draft-matrix.message-box.coverage";
import { coverage as threadBox } from "../../features/inbox/draft-matrix.thread-box.coverage";
import { coverage as margin } from "../../features/margin/draft-matrix.margin.coverage";
import { type CensusItem, composerCensus, keyParts } from "./census";
import type { CoverageEntry } from "./coverage";
import { testTitles } from "./test-titles";

const WEB_SRC = join(import.meta.dir, "..", "..");

/** The draft matrix files, under `web/src`, with their coverage lists. */
const MATRIX: readonly { readonly file: string; readonly entries: readonly CoverageEntry[] }[] = [
  { entries: signIn, file: "draft-matrix.sign-in.test.tsx" },
  { entries: agentsRow, file: "features/agents/draft-matrix.agents-row.test.tsx" },
  { entries: docked, file: "features/conversation/draft-matrix.docked.test.tsx" },
  { entries: edit, file: "features/conversation/draft-matrix.edit.test.tsx" },
  { entries: inlineReply, file: "features/conversation/draft-matrix.inline-reply.test.tsx" },
  { entries: issuePage, file: "features/conversation/draft-matrix.issue-page.test.tsx" },
  { entries: phoneThread, file: "features/conversation/draft-matrix.phone-thread.test.tsx" },
  { entries: messageBox, file: "features/inbox/draft-matrix.message-box.test.tsx" },
  { entries: threadBox, file: "features/inbox/draft-matrix.thread-box.test.tsx" },
  { entries: margin, file: "features/margin/draft-matrix.margin.test.tsx" },
];

const { items, unread } = composerCensus(process.env.COMPOSER_CENSUS_ROOT ?? WEB_SRC);
const byKey = new Map(items.map((item) => [item.key, item]));

const listOf = (file: string) => file.replace(/\.test\.tsx$/, ".coverage.ts");

/** An item as a failure names it: its host, the mount or component it is about, its kind and
 *  expression, and where it is. */
function named(item: CensusItem): string {
  const detail = item.detail === "" ? "" : `; ${item.detail}`;
  return `${item.host} · ${item.mount} · ${item.kind} · ${item.expression} (${item.at.join(", ")}${detail})`;
}

/** The matrix files an item belongs in. A file answers for a mount where it answers for that
 *  mount's lifetime, and for a place (a host's code, about one component) where it answers for an
 *  item there. So: the files answering at the same place above one of the same mounts that also
 *  answer for one of those mounts; else any file answering at the same place above one of the
 *  same mounts, as the issue page's file does above the Conversation's; else at the same place;
 *  else for one of the mounts. */
function homesOf(item: CensusItem): string[] {
  const filesWhere = (rule: (other: CensusItem) => boolean) =>
    MATRIX.filter(({ entries }) =>
      entries.some((entry) => {
        const other = byKey.get(entry.key);
        return other !== undefined && rule(other);
      })
    ).map(({ file }) => file);
  const samePlace = (other: CensusItem) => other.host === item.host && other.mount === item.mount;
  const sameMount = (other: CensusItem) => other.above.some((name) => item.above.includes(name));
  const placed = filesWhere((other) => samePlace(other) && sameMount(other));
  const owners = filesWhere((other) => other.kind === "lifetime" && sameMount(other));
  const both = placed.filter((file) => owners.includes(file));
  return [both, placed, filesWhere(samePlace), owners].find((files) => files.length > 0) ?? [];
}

/** An entry's item as a failure names it, read back from its key. */
function entryNamed(entry: CoverageEntry): string {
  const { expression, host, kind, mount } = keyParts(entry.key);
  return `${host} · ${mount} · ${kind} · ${expression}`;
}

test("the composer census places every mount and reads everything between a mount and the routes", () => {
  expect(unread).toEqual([]);
});

test("every item the composer census finds has a draft matrix row or a numbered reason", () => {
  const answered = new Set(MATRIX.flatMap(({ entries }) => entries.map((entry) => entry.key)));
  const missing = items
    .filter((item) => !answered.has(item.key))
    .map((item) => {
      const homes = homesOf(item).map(listOf);
      return homes.length > 0
        ? `${named(item)} has no row and no reason; it belongs in ${homes.join(" or ")}`
        : `${named(item)} has no row and no reason, and no matrix file answers for ${item.above.join(", ")}: it belongs in a draft matrix file of its own`;
    });
  expect(missing).toEqual([]);
});

test("every coverage entry names an item the composer census still finds", () => {
  const stale = MATRIX.flatMap(({ entries, file }) =>
    entries
      .filter((entry) => !byKey.has(entry.key))
      .map((entry) => `${listOf(file)}: ${entryNamed(entry)} is no item of the census`)
  );
  expect([...new Set(stale)]).toEqual([]);
});

test("every row entry names a test of its draft matrix file", () => {
  const wrong: string[] = [];
  for (const { entries, file } of MATRIX) {
    const { titles, unread } = testTitles(join(WEB_SRC, file));
    for (const line of unread) wrong.push(`${file}, ${line}`);
    for (const entry of entries) {
      if (!("row" in entry) || titles.has(entry.row)) continue;
      wrong.push(
        `${listOf(file)}: "${entry.row}", for ${entryNamed(entry)}, is no test of ${file}`
      );
    }
  }
  expect(wrong).toEqual([]);
});

test("every reason entry has words", () => {
  const empty = MATRIX.flatMap(({ entries, file }) =>
    entries
      .filter((entry) => "reason" in entry && !/\p{L}{2,}/u.test(entry.reason))
      .map((entry) => `${listOf(file)}: the reason for ${entryNamed(entry)} has no words`)
  );
  expect(empty).toEqual([]);
});
